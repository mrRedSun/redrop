package sharing

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/internal/apperr"
)

// rawTarBlock builds one ustar header block, so tests can write layouts that
// archive/tar's writer never produces.
func rawTarBlock(name string, typ byte, size int64) []byte {
	b := make([]byte, 512)
	copy(b, name)
	copy(b[100:], "0000644\x00")
	copy(b[108:], "0000000\x00")
	copy(b[116:], "0000000\x00")
	copy(b[124:], fmt.Sprintf("%011o\x00", size))
	copy(b[136:], "00000000000\x00")
	b[156] = typ
	copy(b[257:], "ustar\x0000")
	rawTarChecksum(b)
	return b
}

func rawTarChecksum(b []byte) {
	for i := 148; i < 156; i++ {
		b[i] = ' '
	}
	sum := 0
	for _, c := range b[:512] {
		sum += int(c)
	}
	copy(b[148:], fmt.Sprintf("%06o\x00 ", sum))
}

func rawTarPad(b []byte) []byte { return append(b, make([]byte, (512-len(b)%512)%512)...) }

func rawPAX(typ byte, records ...string) []byte {
	var body []byte
	for _, r := range records {
		n := len(r) + 3
		for len(fmt.Sprintf("%d %s\n", n, r)) != n {
			n++
		}
		body = append(body, fmt.Sprintf("%d %s\n", n, r)...)
	}
	return append(rawTarBlock("PaxHeader", typ, int64(len(body))), rawTarPad(body)...)
}

// rawHiddenBody is a NUL-type entry whose 64 MiB "body" covers the headers
// after it if the counter misreads it as a regular file.
func rawHiddenBody(name string) []byte { return rawTarBlock(name, 0, 64<<20) }

func rawPrefixDir() []byte {
	b := rawTarBlock("", 0, 64<<20)
	copy(b[345:], "pp")
	rawTarChecksum(b)
	return b
}

func rawArchive(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return append(b, make([]byte, 1024)...)
}

func gzipBytes(t testing.TB, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// desyncCases are review 27's five layouts (B-1) where archive/tar returns a
// directory that the counter treats as the start of a large body.
func desyncCases() []struct {
	name  string
	parts [][]byte
} {
	return []struct {
		name  string
		parts [][]byte
	}{
		{"A stale PAX directory path", [][]byte{rawPAX('x', "path="+strings.Repeat("x", 120)+"/"), rawTarBlock("ignored", '5', 0), rawHiddenBody("f")}},
		{"B GNU name precedes PAX", [][]byte{rawTarBlock("././@LongLink", 'L', 3), rawTarPad([]byte("aa/")), rawPAX('x', "path=aa"), rawHiddenBody("aa")}},
		{"C replacing PAX map", [][]byte{rawPAX('x', "path=zz/"), rawPAX('x', "comment=c"), rawHiddenBody("f")}},
		{"D empty PAX map", [][]byte{rawPAX('x', "path=zz/"), rawTarBlock("PaxHeader", 'x', 0), rawHiddenBody("f")}},
		{"E USTAR prefix", [][]byte{rawPrefixDir()}},
	}
}

func TestArchiveCounterDesyncRejected(t *testing.T) {
	for _, tc := range append(desyncCases(), struct {
		name  string
		parts [][]byte
	}{"control", nil}) {
		t.Run(tc.name, func(t *testing.T) {
			s, a := budgetService(t, 4<<20, 8<<20)
			parts := [][]byte{rawTarBlock("index.html", '0', 5), rawTarPad([]byte("hello"))}
			parts = append(parts, tc.parts...)
			for i := range 300 { // more directories than the 128-entry cap
				parts = append(parts, rawTarBlock(fmt.Sprintf("d%03d/", i), '5', 0))
			}
			c := budgetContent(t, s, a, gzipBytes(t, rawArchive(parts...)))
			_, err := s.Create(context.Background(), a, c, Settings{})
			want := "inconsistent entry framing"
			if tc.name == "control" {
				want = "more than 128 archive entries"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%v, want %q", err, want)
			}
		})
	}
}

// m-E: repeated directories (tar -C dist . -C static .) and PAX-format sites
// with MaxFiles files and their directories, as main accepted.
func TestArchiveRepeatedDirectoriesAndPAXSite(t *testing.T) {
	for _, format := range []tar.Format{tar.FormatGNU, tar.FormatPAX} {
		t.Run(format.String(), func(t *testing.T) {
			s, a := budgetService(t, 4<<20, 8<<20)
			s.cfg.MaxFiles = 100
			var headers []tar.Header
			var bodies [][]byte
			add := func(name string, body []byte) {
				h := tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg, Format: format}
				if strings.HasSuffix(name, "/") {
					h.Typeflag, h.Mode = tar.TypeDir, 0o755
				}
				if format == tar.FormatPAX {
					h.PAXRecords = map[string]string{"mtime": "1600000000.5"}
				}
				headers, bodies = append(headers, h), append(bodies, body)
			}
			for _, tree := range []string{"a", "b"} { // two trees that share css/
				add("./", nil)
				add("./css/", nil)
				add("./css/"+tree+".css", []byte(tree))
			}
			for i := range 98 {
				add(fmt.Sprintf("./d%d/", i), nil)
				add(fmt.Sprintf("./d%d/f", i), []byte("x"))
			}
			art, err := s.Create(context.Background(), a, budgetContent(t, s, a, budgetArchive(t, headers, bodies)), Settings{})
			if err != nil {
				t.Fatal(err)
			}
			if art.Files != 100 {
				t.Fatalf("files=%d, want 100", art.Files)
			}
		})
	}
}

func TestArchiveDirectoryNamesAreNotValidated(t *testing.T) {
	s, a := budgetService(t, 4<<20, 8<<20)
	raw := rawArchive(rawTarBlock("caf\xe9/", '5', 0), rawTarBlock("index.html", '0', 2), rawTarPad([]byte("ok")))
	if _, err := s.Create(context.Background(), a, budgetContent(t, s, a, gzipBytes(t, raw)), Settings{}); err != nil {
		t.Fatalf("Latin-1 empty directory, accepted on main: %v", err)
	}
}

func TestArchiveFileDirectoryCollision(t *testing.T) {
	for _, names := range [][]string{{"a", "a/b"}, {"a/b/c", "a/b"}, {"x/y", "x-y", "x"}} {
		t.Run(strings.Join(names, ","), func(t *testing.T) {
			s, a := budgetService(t, 4<<20, 8<<20)
			var headers []tar.Header
			var bodies [][]byte
			for _, n := range names {
				headers = append(headers, tar.Header{Name: n, Size: 1, Mode: 0o644})
				bodies = append(bodies, []byte("x"))
			}
			_, err := s.Create(context.Background(), a, budgetContent(t, s, a, budgetArchive(t, headers, bodies)), Settings{})
			if !apperr.Is(err, apperr.KindInvalid) || !strings.Contains(err.Error(), "both a file and a directory") {
				t.Fatalf("%v, want a file/directory collision", err)
			}
		})
	}
	s, a := budgetService(t, 4<<20, 8<<20)
	headers := []tar.Header{{Name: "a-b", Size: 1, Mode: 0o644}, {Name: "a/b", Size: 1, Mode: 0o644}, {Name: "a.b", Size: 1, Mode: 0o644}}
	if _, err := s.Create(context.Background(), a, budgetContent(t, s, a, budgetArchive(t, headers, [][]byte{{1}, {2}, {3}})), Settings{}); err != nil {
		t.Fatalf("siblings sharing a prefix: %v", err)
	}
}

// checkArchiveFraming runs data through archive/tar alone and through the work
// reader. Whenever a header passes inSync, as extraction requires, the two
// must agree on the header and its bytes, and the counter must have counted
// every non-metadata entry archive/tar returned so far.
func checkArchiveFraming(t testing.TB, data []byte) {
	reference := tar.NewReader(bytes.NewReader(data))
	work := &archiveWorkReader{r: bytes.NewReader(data), remaining: 64 << 20, maxEntries: 1 << 20}
	checked := tar.NewReader(work)
	returned := 0
	for {
		got, gerr := checked.Next()
		want, werr := reference.Next()
		if gerr != nil {
			if gerr == io.EOF && werr != io.EOF {
				t.Fatalf("counter ended the archive early; archive/tar: %v", werr)
			}
			return // extraction fails here, which is always safe
		}
		if werr != nil {
			t.Fatalf("checked reader returned a header archive/tar rejected: %v", werr)
		}
		if !work.inSync(got) {
			return // extraction refuses the archive here
		}
		if got.Typeflag != tar.TypeXGlobalHeader {
			returned++
		}
		if work.entries != returned {
			t.Fatalf("counter saw %d entries, archive/tar returned %d", work.entries, returned)
		}
		if got.Name != want.Name || got.Size != want.Size || got.Typeflag != want.Typeflag {
			t.Fatalf("header %q/%d/%c, archive/tar %q/%d/%c", got.Name, got.Size, got.Typeflag, want.Name, want.Size, want.Typeflag)
		}
		a, aerr := io.ReadAll(checked)
		b, berr := io.ReadAll(reference)
		if aerr != nil {
			return // the byte budget ended extraction
		}
		if berr != nil || !bytes.Equal(a, b) {
			t.Fatalf("payload differs: %v", berr)
		}
	}
}

// archiveSeeds are valid archives in each format, with long names, USTAR
// prefixes, directories, PAX records and global headers, plus the desyncs.
func archiveSeeds(t testing.TB) [][]byte {
	var seeds [][]byte
	long := strings.Repeat("n", 90) + "/" + strings.Repeat("m", 60) // USTAR prefix split
	for _, format := range []tar.Format{tar.FormatUSTAR, tar.FormatPAX, tar.FormatGNU} {
		var raw bytes.Buffer
		tw := tar.NewWriter(&raw)
		write := func(h *tar.Header, body []byte) {
			h.Format = format
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
		if format == tar.FormatPAX {
			write(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "g", PAXRecords: map[string]string{"comment": "global"}}, nil)
		}
		write(&tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755}, nil)
		write(&tar.Header{Name: "dir/index.html", Mode: 0o644, Size: 700}, bytes.Repeat([]byte("h"), 700))
		write(&tar.Header{Name: long + "/", Typeflag: tar.TypeDir, Mode: 0o755}, nil)
		write(&tar.Header{Name: long + "/f", Mode: 0o644, Size: 512}, bytes.Repeat([]byte("f"), 512))
		if format != tar.FormatUSTAR {
			write(&tar.Header{Name: strings.Repeat("x", 300), Mode: 0o644, Size: 3}, []byte("abc"))
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		seeds = append(seeds, raw.Bytes())
	}
	for _, tc := range desyncCases() {
		seeds = append(seeds, rawArchive(append(tc.parts, rawTarBlock("d/", '5', 0), rawTarBlock("e/", '5', 0))...))
	}
	return seeds
}

// mutateArchive changes one framing-relevant header field of a 512-byte block
// and fixes its checksum, so archive/tar parses the result instead of
// rejecting it outright.
func mutateArchive(rng *rand.Rand, seed []byte) []byte {
	data := append([]byte{}, seed...)
	blocks := len(data) / 512
	if blocks == 0 {
		return data
	}
	for range 1 + rng.Intn(3) {
		b := data[rng.Intn(blocks)*512:][:512]
		switch rng.Intn(5) {
		case 0:
			b[156] = "0\x0025xgLK7S"[rng.Intn(10)]
		case 1: // name: trailing slash or not
			n := bytes.IndexByte(b[:100], 0)
			if n < 0 {
				n = 100
			}
			if n > 0 && b[n-1] == '/' {
				b[n-1] = 'z'
			} else if n < 100 {
				b[n] = '/'
			}
		case 2:
			copy(b[124:136], fmt.Sprintf("%011o\x00", rng.Intn(4096)))
		case 3:
			copy(b[345:], []string{"pp\x00", "\x00", "a/b\x00"}[rng.Intn(3)])
		case 4:
			copy(b[257:265], []string{"ustar\x0000", "ustar  \x00", "\x00\x00\x00\x00\x00\x00\x00\x00"}[rng.Intn(3)])
		}
		rawTarChecksum(b)
	}
	return data
}

// FuzzArchiveFraming checks the work reader against archive/tar. Plain
// `go test` runs the seeds; `go test -fuzz FuzzArchiveFraming` explores more.
func FuzzArchiveFraming(f *testing.F) {
	for _, seed := range archiveSeeds(f) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) { checkArchiveFraming(t, data) })
}

// TestArchiveFramingDifferential runs a bounded, deterministic set of header
// mutations through the same differential check on every go test.
func TestArchiveFramingDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(27))
	seeds := archiveSeeds(t)
	for i := range 3000 {
		data := mutateArchive(rng, seeds[i%len(seeds)])
		if !t.Run(fmt.Sprint(i), func(t *testing.T) { checkArchiveFraming(t, data) }) {
			t.FailNow()
		}
	}
}

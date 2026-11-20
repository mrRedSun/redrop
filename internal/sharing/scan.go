package sharing

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"

	"github.com/mrRedSun/redrop/internal/store"
)

// Upload checks (DESIGN.md "Abuse handling", "Upload checks"): after a
// publication commits, its files are scanned for a password form that posts
// to another host and for executables. A finding becomes a system report.
// The checks only flag: they never change, block or fail a publication.

// Scan bounds. Variables so tests can shrink them.
var (
	scanTimeout    = 5 * time.Second
	scanTotalBytes = int64(32 << 20) // read from all files together
	scanHTMLBytes  = int64(1 << 20)  // read from one HTML file
)

const (
	scanMagicBytes = 4096 // read from every file for magic numbers
	maxFindingPath = 200
)

// Finding kinds.
const (
	findingForm       = "form"
	findingExecutable = "executable"
)

// finding is one upload check result.
type finding struct {
	kind     string
	category string
	detail   string
}

// checkUpload scans a committed version of artifactID and stores a system
// report for each finding. Any failure is logged and ignored. It runs under
// its own deadline, detached from the publishing request.
func (s *Service) checkUpload(ctx context.Context, artifactID string, version int, files []store.File) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scanTimeout)
	defer cancel()
	a, err := s.db.ArtifactByID(ctx, artifactID)
	if err != nil {
		s.log.Warn("upload check skipped", "id", artifactID, "error", err)
		return
	}
	host := a.Slug + "." + s.cfg.ContentHost()
	found, complete := scanFiles(ctx, files, host, func(sum string) (io.ReadCloser, error) { return s.blobs.Open(sum) })
	if !complete {
		s.log.Warn("upload check incomplete", "id", artifactID, "version", version)
	}
	for _, f := range found {
		r := &store.Report{ArtifactID: a.ID, OwnerID: a.UserID, Version: version, Kind: f.kind, Category: f.category, Details: f.detail}
		stored, err := s.db.AddSystemReport(ctx, r)
		if err != nil {
			s.log.Error("upload flag", "id", artifactID, "error", err)
			continue
		}
		s.log.Info("upload flagged", "id", artifactID, "version", version, "kind", f.kind, "new", stored)
		if stored {
			s.announce(r, a.OwnerUsername)
		}
	}
}

// scanFiles runs the checks on files of the artifact served at host,
// reading blobs through open. It stops at the first finding of each kind,
// and when ctx ends or the byte budget is spent, in which case complete is
// false.
func scanFiles(ctx context.Context, files []store.File, host string, open func(blob string) (io.ReadCloser, error)) (found []finding, complete bool) {
	budget := scanTotalBytes
	var form, exe bool
	for _, f := range files {
		if form && exe {
			break
		}
		if ctx.Err() != nil || budget <= 0 {
			return found, false
		}
		if !exe {
			if what := executableName(f.Path); what != "" {
				exe = true
				found = append(found, finding{findingExecutable, CategoryMalware, findingPath(f.Path) + ": " + what})
			}
		}
		htmlFile := !form && isHTML(f)
		if exe && !htmlFile {
			continue
		}
		n := min(f.Size, int64(scanMagicBytes))
		if htmlFile {
			n = min(f.Size, scanHTMLBytes)
		}
		n = min(n, budget)
		if n <= 0 {
			continue
		}
		head, err := readHead(open, f.Blob, n)
		budget -= n
		if err != nil {
			return found, false
		}
		if !exe {
			if what := executableMagic(head); what != "" {
				exe = true
				found = append(found, finding{findingExecutable, CategoryMalware, findingPath(f.Path) + ": " + what})
			}
		}
		if htmlFile {
			if target := credentialForm(bytes.NewReader(head), host); target != "" {
				form = true
				found = append(found, finding{findingForm, CategoryPhishing, findingPath(f.Path) + ": a password form posts to " + target})
			}
		}
	}
	return found, true
}

func readHead(open func(string) (io.ReadCloser, error), blob string, n int64) ([]byte, error) {
	rc, err := open(blob)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, n))
}

// findingPath is a file path as a finding shows it: one cleaned line,
// shortened. Uploaders choose file names, so this is untrusted text.
func findingPath(p string) string {
	p = cleanText(p, false)
	if utf8.RuneCountInString(p) > maxFindingPath {
		p = string([]rune(p)[:maxFindingPath]) + "…"
	}
	return p
}

func isHTML(f store.File) bool {
	ext := strings.ToLower(path.Ext(f.Path))
	return ext == ".html" || ext == ".htm" || strings.HasPrefix(f.ContentType, "text/html")
}

// executableExtensions are installers, programs and scripts that run when
// opened on Windows, Android, macOS or Linux desktops.
var executableExtensions = map[string]string{
	".exe": "Windows program", ".msi": "Windows installer", ".msix": "Windows installer", ".appx": "Windows installer",
	".msp": "Windows installer patch", ".scr": "Windows screen saver (a program)", ".com": "DOS program", ".pif": "Windows program shortcut",
	".cpl": "Windows control panel item", ".dll": "Windows library", ".bat": "Windows batch script", ".cmd": "Windows batch script",
	".vbs": "Windows script", ".vbe": "Windows script", ".wsf": "Windows script", ".hta": "HTML application", ".lnk": "Windows shortcut",
	".jar": "Java program", ".apk": "Android app", ".xapk": "Android app", ".apks": "Android app", ".ipa": "iOS app",
	".dmg": "macOS disk image", ".pkg": "macOS installer", ".deb": "Debian package", ".rpm": "RPM package", ".appimage": "Linux AppImage",
}

func executableName(p string) string {
	if what, ok := executableExtensions[strings.ToLower(path.Ext(p))]; ok {
		return what + " (" + strings.ToLower(path.Ext(p)) + ")"
	}
	return ""
}

// executableMagic recognizes PE, ELF and Mach-O images by their first bytes.
func executableMagic(b []byte) string {
	switch {
	case len(b) >= 0x40 && b[0] == 'M' && b[1] == 'Z':
		off := int64(binary.LittleEndian.Uint32(b[0x3c:]))
		if off+4 <= int64(len(b)) && string(b[off:off+4]) == "PE\x00\x00" {
			return "Windows executable (PE)"
		}
	case bytes.HasPrefix(b, []byte("\x7fELF")):
		return "Linux executable (ELF)"
	case len(b) >= 4:
		switch binary.BigEndian.Uint32(b) {
		case 0xfeedface, 0xfeedfacf, 0xcefaedfe, 0xcffaedfe:
			return "macOS executable (Mach-O)"
		case 0xcafebabe, 0xcafebabf:
			// Java class files share this magic; a universal binary's
			// next word is its small architecture count.
			if len(b) >= 8 {
				if n := binary.BigEndian.Uint32(b[4:]); n > 0 && n < 32 {
					return "macOS universal executable (Mach-O)"
				}
			}
		}
	}
	return ""
}

// credentialForm reports the host a password form in the document posts
// to, when that is another host than host (the artifact's own), or "".
// A form counts when it holds an <input type=password> and its action, or
// a formaction on one of its controls, is an absolute or protocol-relative
// http(s) URL on another host.
func credentialForm(r io.Reader, host string) string {
	type form struct {
		password bool
		targets  []string
	}
	var cur *form
	check := func() string {
		if cur == nil || !cur.password {
			return ""
		}
		for _, t := range cur.targets {
			if h := foreignHost(t, host); h != "" {
				return h
			}
		}
		return ""
	}
	z := html.NewTokenizer(r)
	for {
		switch z.Next() {
		case html.ErrorToken: // end of input, or of the bytes read
			return check()
		case html.StartTagToken, html.SelfClosingTagToken:
			name, more := z.TagName()
			tag := string(name)
			if tag != "form" && (cur == nil || (tag != "input" && tag != "button")) {
				continue
			}
			attrs := map[string]string{}
			for more {
				var k, v []byte
				k, v, more = z.TagAttr()
				if _, seen := attrs[string(k)]; !seen { // the first occurrence wins, as in browsers
					attrs[string(k)] = string(v)
				}
			}
			switch tag {
			case "form":
				if h := check(); h != "" {
					return h
				}
				cur = &form{targets: []string{attrs["action"]}}
			default:
				if tag == "input" && strings.EqualFold(strings.TrimSpace(attrs["type"]), "password") {
					cur.password = true
				}
				if fa, ok := attrs["formaction"]; ok {
					cur.targets = append(cur.targets, fa)
				}
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "form" {
				if h := check(); h != "" {
					return h
				}
				cur = nil
			}
		}
	}
}

// foreignHost returns the host of target, a form action, when it is an
// http(s) URL on a host other than own; "" otherwise (relative, same host,
// or another scheme).
func foreignHost(target, own string) string {
	t := strings.ReplaceAll(strings.TrimSpace(target), `\`, "/") // browsers read \ as / in http(s) URLs
	u, err := url.Parse(t)
	if err != nil || u.Host == "" || (u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	h := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if h == "" || h == own {
		return ""
	}
	return findingPath(h)
}

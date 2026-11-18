package e2e

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mrRedSun/redrop/pkg/api"
)

// ManifestPath is where a version stores its manifest object. Every other
// stored path is an object name.
const ManifestPath = "manifest"

// objectNameLen is 16 random bytes in unpadded base32.
const objectNameLen = 26

var objectNames = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewObjectName returns a random stored name for one file.
func NewObjectName(rnd io.Reader) (string, error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	b := make([]byte, 16)
	if _, err := io.ReadFull(rnd, b); err != nil {
		return "", err
	}
	return objectNames.EncodeToString(b), nil
}

// ValidObjectName reports whether s is a stored object name.
func ValidObjectName(s string) bool {
	if len(s) != objectNameLen {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// Manifest lists a version's files. It is encrypted like a file, as the
// object at ManifestPath, so the server never sees paths or types.
type Manifest struct {
	Format int            `json:"format"`
	Kind   string         `json:"kind"`  // html, file, or bundle
	Title  string         `json:"title"` // the page title or file name, for the viewer
	Entry  string         `json:"entry"` // the start page; "" for a bundle without one
	Files  []ManifestFile `json:"files"`
}

// ManifestFile is one file of a version.
type ManifestFile struct {
	Path   string `json:"path"`   // bundle path, as on a plain artifact
	Object string `json:"object"` // stored object name
	Type   string `json:"type"`   // Content-Type
	Size   int64  `json:"size"`   // plaintext bytes
}

// MaxPathLen bounds file paths, as for plain bundles.
const MaxPathLen = 1024

// ValidPath reports whether p is a clean relative bundle path outside the
// reserved prefix, as plain bundles require.
func ValidPath(p string) bool {
	if p == "" || len(p) > MaxPathLen || !utf8.ValidString(p) || strings.ContainsAny(p, "\x00\\") {
		return false
	}
	if path.Clean("/"+p) != "/"+p || strings.HasSuffix(p, "/") {
		return false
	}
	return p != api.ReservedPrefix && !strings.HasPrefix(p, api.ReservedPrefix+"/")
}

// Validate checks a manifest's structure: known format and kind, valid
// unique paths and object names, no path that is also another's directory,
// and an entry that is one of the files.
func (m *Manifest) Validate() error {
	if m.Format != FormatVersion {
		return fmt.Errorf("e2e manifest: unknown format %d", m.Format)
	}
	switch m.Kind {
	case api.KindHTML, api.KindFile, api.KindBundle:
	default:
		return fmt.Errorf("e2e manifest: unknown kind %q", m.Kind)
	}
	if len(m.Files) == 0 {
		return errors.New("e2e manifest: no files")
	}
	paths := make([]string, 0, len(m.Files))
	seenPath, seenObject := map[string]bool{}, map[string]bool{}
	for _, f := range m.Files {
		switch {
		case !ValidPath(f.Path):
			return fmt.Errorf("e2e manifest: invalid path %q", f.Path)
		case !ValidObjectName(f.Object):
			return fmt.Errorf("e2e manifest: invalid object name %q", f.Object)
		case seenPath[f.Path] || seenObject[f.Object]:
			return fmt.Errorf("e2e manifest: duplicate path or object %q", f.Path)
		case f.Size < 0 || f.Type == "":
			return fmt.Errorf("e2e manifest: invalid size or type for %q", f.Path)
		}
		seenPath[f.Path], seenObject[f.Object] = true, true
		paths = append(paths, f.Path)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if i := sort.SearchStrings(paths, p+"/"); i < len(paths) && strings.HasPrefix(paths[i], p+"/") {
			return fmt.Errorf("e2e manifest: %q is both a file and a directory", p)
		}
	}
	if m.Entry != "" && !seenPath[m.Entry] {
		return fmt.Errorf("e2e manifest: entry %q is not a file", m.Entry)
	}
	if m.Entry == "" && m.Kind != api.KindBundle {
		return errors.New("e2e manifest: a page or file needs its entry")
	}
	return nil
}

// Marshal validates and encodes the manifest.
func (m *Manifest) Marshal() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// ParseManifest decodes and validates a manifest's plaintext.
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("e2e manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

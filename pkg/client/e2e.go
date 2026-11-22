package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mrRedSun/redrop/internal/contenttype"
	"github.com/mrRedSun/redrop/internal/e2e"
	"github.com/mrRedSun/redrop/pkg/api"
)

// End-to-end encrypted publishing (DESIGN.md "End-to-end encrypted links").
// The source is prepared exactly as for a plaintext share (folder rules,
// secret filtering, Markdown rendering), then every resulting file is
// encrypted on this machine and uploaded as a bundle of a manifest and
// opaque objects. The key never leaves the client: requests carry only its
// key check.

// E2EKey is an artifact's end-to-end link key.
type E2EKey = e2e.Key

// NewE2EKey returns a fresh random key.
func NewE2EKey() (E2EKey, error) { return e2e.NewKey(rand.Reader) }

// ParseE2EKey reads a key in its link form (43 characters of base64url).
func ParseE2EKey(s string) (E2EKey, error) { return e2e.ParseKey(s) }

// E2ELink returns the artifact link with key in its fragment: the link to
// give people. The server never sees the fragment.
func E2ELink(artifactURL string, key E2EKey) string { return e2e.Link(artifactURL, key) }

// E2EKeyFromLink returns the key in a link's #k= fragment; ok is false when
// the link has none.
func E2EKeyFromLink(link string) (key E2EKey, ok bool, err error) { return e2e.KeyFromLink(link) }

// ErrE2EKeyMismatch means a key is not the artifact's.
var ErrE2EKeyMismatch = errors.New("this key does not belong to the artifact; use its full link (with #k=…) or the key saved when it was shared")

// ErrEncryptedArtifact means a plaintext version was about to be published
// to an end-to-end encrypted artifact. Nothing was uploaded: publish it
// with the artifact's key (UpdateE2E, E2EUploader).
var ErrEncryptedArtifact = errors.New("the artifact is end-to-end encrypted; its new versions must be encrypted with its key")

// checkPlaintextTarget reads the artifact before a plaintext version is
// uploaded, so plaintext meant for an end-to-end encrypted artifact never
// leaves this machine. The server refuses it at upload creation too.
func (c *Client) checkPlaintextTarget(ctx context.Context, artifactID string) error {
	a, err := c.GetArtifact(ctx, artifactID)
	if err != nil {
		return err
	}
	if a.E2E {
		return &UsageError{Message: fmt.Sprintf("artifact %s is end-to-end encrypted: publish with its key (redrop update and watch use the key from its full link or this machine's key store); nothing was uploaded", a.ID), Cause: ErrEncryptedArtifact}
	}
	return nil
}

// e2eAttempts bounds encrypting again after another version took the
// number the content was encrypted for.
const e2eAttempts = 3

// ReserveArtifact mints an artifact ID for an end-to-end share.
func (c *Client) ReserveArtifact(ctx context.Context) (*api.ArtifactReservation, error) {
	var out api.ArtifactReservation
	return &out, c.do(ctx, http.MethodPost, "/api/v1/artifacts/reservations", nil, &out)
}

// ShareE2E prepares src, encrypts it with a new key, uploads it, and
// publishes it as a new end-to-end encrypted artifact. remember, if set, is
// called with the reserved artifact ID and the key before anything is
// uploaded; its error aborts the share, so a key that could not be saved
// never protects published content. The returned key is the only way to
// read the artifact: give out E2ELink(a.URL, key).
func (c *Client) ShareE2E(ctx context.Context, src *Source, opts PublishOptions, remember func(id string, key E2EKey) error, progress Progress) (*api.Artifact, E2EKey, error) {
	var key E2EKey
	if src.SPA {
		return nil, key, usageErrorf("--spa is not available with --e2e: the server can't read paths to fall back on")
	}
	key, err := NewE2EKey()
	if err != nil {
		return nil, key, err
	}
	res, err := c.ReserveArtifact(ctx)
	if err != nil {
		return nil, key, err
	}
	if remember != nil {
		if err := remember(res.ID, key); err != nil {
			return nil, key, err
		}
	}
	u, err := retrySourceChange(func() (*upload, error) {
		return c.prepareEncryptAndUpload(ctx, src, key, res.ID, 1, UploadTarget{E2E: true}, progress, nil)
	})
	if err != nil {
		return nil, key, err
	}
	req := u.e2eRequest(src, key, res.ID, 1)
	req.E2E.Reservation = res.Token
	a, err := c.CreateArtifact(ctx, &api.CreateArtifactRequest{
		PublishRequest: req,
		Title:          opts.Title, TTL: opts.TTL, Password: opts.Password, MaxViews: opts.MaxViews,
		InfoButton: opts.InfoButton, OldVersions: opts.OldVersions, Feedback: opts.Feedback,
	})
	return a, key, err
}

// UpdateE2E prepares src, applies opts, then publishes it as a new version
// of an end-to-end encrypted artifact, encrypted with key. The link stays the
// same. A failure after opts were applied is an *ErrSettingsOnly.
func (c *Client) UpdateE2E(ctx context.Context, artifactID string, key E2EKey, src *Source, opts VersionOptions, progress Progress) (*api.Artifact, error) {
	applied := false
	apply := func() error {
		if applied || (opts.InfoButton == nil && opts.OldVersions == nil && opts.Feedback == nil) {
			return nil
		}
		if _, err := c.UpdateArtifact(ctx, artifactID, &api.UpdateArtifactRequest{InfoButton: opts.InfoButton, OldVersions: opts.OldVersions, Feedback: opts.Feedback}); err != nil {
			return err
		}
		applied = true
		return nil
	}
	_, a, err := c.publishE2E(ctx, artifactID, key, src, progress, apply, true)
	if err != nil && applied {
		return nil, &ErrSettingsOnly{Err: err}
	}
	return a, err
}

// E2EUploader publishes end-to-end encrypted versions for Watch.
type E2EUploader struct {
	Client *Client
	Key    E2EKey
}

// UpdateDigest is Client.UpdateDigest for an end-to-end encrypted artifact:
// one publish without source-change retries, whose digest is the source's,
// not the ciphertext's.
func (u *E2EUploader) UpdateDigest(ctx context.Context, artifactID string, src *Source, progress Progress) (string, *api.Artifact, error) {
	return u.Client.publishE2E(ctx, artifactID, u.Key, src, progress, nil, false)
}

// CheckE2EKey reports whether key is the end-to-end artifact a's key.
func CheckE2EKey(a *api.Artifact, key E2EKey) error {
	if !a.E2E {
		return usageErrorf("artifact %s is not end-to-end encrypted; publish it without --e2e", a.ID)
	}
	if a.E2EKeyCheck == "" || a.E2EKeyCheck != key.Check(a.ID) {
		return &UsageError{Message: ErrE2EKeyMismatch.Error(), Cause: ErrE2EKeyMismatch}
	}
	return nil
}

// publishE2E encrypts src for the artifact's next version and publishes it,
// starting over when another version takes that number first.
func (c *Client) publishE2E(ctx context.Context, artifactID string, key E2EKey, src *Source, progress Progress, before func() error, retrySource bool) (string, *api.Artifact, error) {
	if src.SPA {
		return "", nil, usageErrorf("--spa is not available with --e2e: the server can't read paths to fall back on")
	}
	for attempt := 1; ; attempt++ {
		a, err := c.GetArtifact(ctx, artifactID)
		if err != nil {
			return "", nil, err
		}
		if err := CheckE2EKey(a, key); err != nil {
			return "", nil, err
		}
		step := func() (*upload, error) {
			return c.prepareEncryptAndUpload(ctx, src, key, a.ID, a.NextVersion, UploadTarget{ArtifactID: a.ID, E2E: true}, progress, before)
		}
		var u *upload
		if retrySource {
			u, err = retrySourceChange(step)
		} else {
			u, err = step()
		}
		if err != nil {
			return "", nil, err
		}
		req := u.e2eRequest(src, key, a.ID, a.NextVersion)
		out, err := c.CreateVersion(ctx, artifactID, &req)
		var e *Error
		if errors.As(err, &e) && e.Status == http.StatusConflict && e.Title == api.CodeVersionConflict && attempt < e2eAttempts {
			continue // encrypt again for the new number
		}
		if err != nil {
			return "", nil, err
		}
		return u.digest, out, nil
	}
}

// e2eRequest is the publication of an encrypted upload. It has no file name
// or entry: the manifest holds them, encrypted.
func (u *upload) e2eRequest(src *Source, key E2EKey, artifactID string, version int) api.PublishRequest {
	return api.PublishRequest{UploadID: u.id, SHA256: u.blobSum, Kind: u.kind, Note: src.Note,
		E2E: &api.E2EPublish{KeyCheck: key.Check(artifactID), Version: version}}
}

// prepareEncryptAndUpload is prepareAndUpload with the prepared files
// encrypted for (artifactID, version) before the upload.
func (c *Client) prepareEncryptAndUpload(ctx context.Context, src *Source, key E2EKey, artifactID string, version int, target UploadTarget, progress Progress, beforeUpload func() error) (*upload, error) {
	p, err := Prepare(src)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	enc, err := encryptPrepared(p, src, key, artifactID, version)
	if err != nil {
		return nil, err
	}
	defer os.Remove(enc.file)
	if beforeUpload != nil {
		if err := beforeUpload(); err != nil {
			return nil, err
		}
	}
	id, blobSum, err := c.UploadFileFor(ctx, enc.file, target, progress)
	if err != nil {
		return nil, err
	}
	if p.skipped > 0 && src.OnSkipped != nil {
		src.OnSkipped(p.skipped)
	}
	return &upload{id: id, blobSum: blobSum, digest: enc.digest, kind: p.Kind}, nil
}

type encrypted struct {
	file   string // the encrypted bundle, a temporary file
	digest string // the source digest, in hashTree's domain
}

// encryptPrepared writes p's files, encrypted, as a bundle of the manifest
// and one object per file. A single html or file source is the one file; a
// bundle's files are its archive entries.
func encryptPrepared(p *Prepared, src *Source, key E2EKey, artifactID string, version int) (_ *encrypted, err error) {
	in, err := openStatedSource(p.File)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	sum := sha256.New()
	read := io.TeeReader(in, sum)
	out, err := os.CreateTemp("", "redrop-e2e-*.tar.gz")
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(out.Name())
		}
	}()
	// Objects are encrypted in source order into a scratch file, then
	// archived in a random order (archive()), so neither the stored rows
	// nor the blobs' write order show the layout.
	scratch, err := os.CreateTemp("", "redrop-e2e-objects-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		scratch.Close()
		os.Remove(scratch.Name())
	}()
	w := &e2eWriter{key: key, artifact: artifactID, version: version, scratch: scratch}
	m := e2e.Manifest{Format: e2e.FormatVersion, Kind: p.Kind}
	switch p.Kind {
	case api.KindHTML, api.KindFile:
		name := "index.html"
		if p.Kind == api.KindFile {
			name = e2eFileName(p.Filename)
		}
		info, err := in.Stat()
		if err != nil {
			return nil, err
		}
		if err := w.add(name, read, info.Size()); err != nil {
			return nil, err
		}
		// A source that grew while it was read changed under us.
		if n, _ := read.Read(make([]byte, 1)); n > 0 {
			return nil, fmt.Errorf("%w: %s grew while it was encrypted", ErrSourceChanged, src.Path)
		}
		m.Entry = name
		m.Title = w.title(name, p.Kind)
	default:
		if err := w.addArchive(read); err != nil {
			return nil, err
		}
		if _, err := io.Copy(io.Discard, read); err != nil { // the rest of the file, for its digest
			return nil, err
		}
		if m.Entry, err = e2eEntry(w.files, p.Entry); err != nil {
			return nil, err
		}
		if m.Entry != "" {
			m.Title = w.title(m.Entry, p.Kind)
		}
	}
	m.Files = w.files
	manifest, err := m.Marshal()
	if err != nil {
		return nil, usageErrorf("%v", err)
	}
	if err := w.seal(e2e.ManifestPath, e2e.ManifestBinding(artifactID, version), bytes.NewReader(manifest), int64(len(manifest))); err != nil {
		return nil, err
	}
	if err := w.archive(out); err != nil {
		return nil, err
	}
	digest := p.Digest
	if digest == "" {
		digest = hex.EncodeToString(sum.Sum(nil)) // the source file as read
	}
	return &encrypted{file: out.Name(), digest: digest}, nil
}

// writeTarGzipLevel is writeTarGzip at a chosen level: ciphertext doesn't
// compress, so encrypted uploads use gzip.NoCompression.
func writeTarGzipLevel(w io.Writer, level int, write func(*tar.Writer) error) (err error) {
	gz, err := gzip.NewWriterLevel(w, level)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	defer func() {
		for _, finish := range []func() error{tw.Close, gz.Close} {
			if closeErr := finish(); err == nil {
				err = closeErr
			}
		}
	}()
	return write(tw)
}

// e2eWriter encrypts objects into a scratch file and keeps the manifest
// entries; archive writes the upload from it.
type e2eWriter struct {
	key      E2EKey
	artifact string
	version  int
	scratch  *os.File
	objects  []sealedObject // in scratch, in source order
	files    []e2e.ManifestFile
	titles   map[string]string // <title> of HTML files, by path
}

// sealedObject is one encrypted object's place in the scratch file.
type sealedObject struct {
	name        string
	offset, len int64
}

// add encrypts n bytes of r as the file at name.
func (w *e2eWriter) add(name string, r io.Reader, n int64) error {
	if !e2e.ValidPath(name) {
		return usageErrorf("%q can't be published end-to-end encrypted: invalid bundle path", name)
	}
	for _, f := range w.files {
		if f.Path == name {
			return usageErrorf("duplicate bundle path %q", name)
		}
	}
	object, err := e2e.NewObjectName(rand.Reader)
	if err != nil {
		return err
	}
	head := &headBuffer{max: titleHead}
	if err := w.seal(object, e2e.FileBinding(w.artifact, w.version, name), io.TeeReader(r, head), n); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: %s ended while it was encrypted", ErrSourceChanged, name)
		}
		return err
	}
	b := head.buf.Bytes()
	w.files = append(w.files, e2e.ManifestFile{Path: name, Object: object, Type: contenttype.For(name, b[:min(len(b), contenttype.SniffLen)]), Size: n})
	if lower := strings.ToLower(name); strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm") {
		if w.titles == nil {
			w.titles = map[string]string{}
		}
		w.titles[name] = htmlTitle(b)
	}
	return nil
}

func (w *e2eWriter) seal(object string, b e2e.Binding, r io.Reader, n int64) error {
	offset, err := w.scratch.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if err := e2e.Seal(w.scratch, r, n, w.key, b, rand.Reader); err != nil {
		return err
	}
	w.objects = append(w.objects, sealedObject{name: object, offset: offset, len: e2e.SealedSize(n)})
	return nil
}

// archive writes every sealed object, the manifest among them, to out as a
// tar.gz in a random order.
func (w *e2eWriter) archive(out io.Writer) (err error) {
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return err
	}
	order := append([]sealedObject(nil), w.objects...)
	mrand.New(mrand.NewChaCha8(seed)).Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	return writeTarGzipLevel(out, gzip.NoCompression, func(tw *tar.Writer) error {
		for _, o := range order {
			if err := tw.WriteHeader(&tar.Header{Name: o.name, Mode: 0o600, Typeflag: tar.TypeReg, Size: o.len}); err != nil {
				return err
			}
			if _, err := io.Copy(tw, io.NewSectionReader(w.scratch, o.offset, o.len)); err != nil {
				return err
			}
		}
		return nil
	})
}

// addArchive encrypts every regular file of a prepared tar.gz bundle,
// with the server's path rules.
func (w *e2eWriter) addArchive(r io.Reader) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return usageErrorf("bundle must be a .tar.gz archive")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return usageErrorf("corrupt bundle archive: %v", err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeXGlobalHeader:
			continue
		case tar.TypeReg:
		default:
			return usageErrorf("bundle may only contain regular files (found a link or special file: %q)", hdr.Name)
		}
		name, err := cleanArchivePath(hdr.Name)
		if err != nil {
			return err
		}
		if err := w.add(name, tr, hdr.Size); err != nil {
			return err
		}
	}
}

// title is the viewer's title for the start page.
func (w *e2eWriter) title(entry, kind string) string {
	if t := w.titles[entry]; t != "" {
		return t
	}
	if kind == api.KindFile {
		return entry
	}
	return ""
}

// cleanArchivePath normalizes an archive entry's path like the server does
// for plain bundles (sharing.CleanBundlePath).
func cleanArchivePath(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", usageErrorf("absolute or invalid path %q in bundle", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", usageErrorf("path escapes the bundle: %q", p)
		}
	}
	c := strings.TrimPrefix(path.Clean("/"+p), "/")
	if c == "" || c == "." || !utf8.ValidString(c) || !e2e.ValidPath(c) {
		return "", usageErrorf("invalid or reserved path %q in bundle", p)
	}
	return c, nil
}

// e2eFileName is the server's file-name rule for a single file
// (sharing.sanitizeFilename), applied here because the server never sees it.
func e2eFileName(name string) string {
	name = path.Base(strings.TrimSpace(strings.ReplaceAll(name, "\\", "/")))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == "/" || name == ".." || strings.HasPrefix(name, api.ReservedPrefix) || !utf8.ValidString(name) {
		return "file"
	}
	for len(name) > 200 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// e2eEntry picks a bundle's start page like the server does: the requested
// entry (Prepared.Entry; an error when it is missing), else index.html,
// else none.
func e2eEntry(files []e2e.ManifestFile, requested string) (string, error) {
	want := "index.html"
	if requested != "" {
		want = strings.TrimPrefix(path.Clean("/"+requested), "/")
	}
	for _, f := range files {
		if f.Path == want {
			return want, nil
		}
	}
	if requested != "" {
		return "", usageErrorf("entry file %s is not in the bundle", want)
	}
	return "", nil
}

const titleHead = 64 << 10

// headBuffer keeps the first max bytes written.
type headBuffer struct {
	buf bytes.Buffer
	max int
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := h.max - h.buf.Len(); room > 0 {
		h.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// htmlTitle is the server's <title> rule for plain HTML.
func htmlTitle(b []byte) string {
	m := titleRE.FindSubmatch(b)
	if m == nil {
		return ""
	}
	t := strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
	for len(t) > 200 {
		_, size := utf8.DecodeLastRuneInString(t)
		t = t[:len(t)-size]
	}
	return t
}

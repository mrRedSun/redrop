// Package client is a Go client for the redrop API. The redrop CLI is built on it.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
)

// Client talks to a redrop server with an API token.
type Client struct {
	BaseURL   string
	Token     string
	HTTP      *http.Client
	UserAgent string
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Token:     token,
		HTTP:      &http.Client{Timeout: 10 * time.Minute},
		UserAgent: "redrop-go-client",
	}
}

// Error is an API error (RFC 9457 problem details).
type Error struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	// RetryAfter is the server's minimum retry delay, parsed from the
	// Retry-After header (delta-seconds or an HTTP date).
	RetryAfter time.Duration `json:"-"`
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	if e.Title != "" {
		return e.Title
	}
	return http.StatusText(e.Status)
}

// NetworkError wraps transport failures.
type NetworkError struct{ Err error }

func (e *NetworkError) Error() string { return "cannot reach server: " + e.Err.Error() }
func (e *NetworkError) Unwrap() error { return e.Err }

// StatusOf returns the HTTP status of an API error, or 0.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (c *Client) send(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &NetworkError{Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return &NetworkError{Err: err}
	}
	if resp.StatusCode >= 400 {
		e := &Error{Status: resp.StatusCode}
		if json.Unmarshal(data, e) != nil || (e.Detail == "" && e.Title == "") {
			e.Detail = strings.TrimSpace(string(data))
			if len(e.Detail) > 300 || strings.HasPrefix(e.Detail, "<") {
				e.Detail = http.StatusText(resp.StatusCode)
			}
		}
		e.Status = resp.StatusCode
		e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		return e
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("unexpected response from server (is the URL a redrop server?): %w", err)
		}
	}
	return nil
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		// Reject negatives and overflow instead of wrapping a large delay.
		if seconds >= 0 && seconds <= int64((1<<63-1)/time.Second) {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.send(req, out)
}

func esc(s string) string { return url.PathEscape(s) }

func (c *Client) AuthConfig(ctx context.Context) (*api.AuthConfig, error) {
	var out api.AuthConfig
	return &out, c.do(ctx, http.MethodGet, "/api/v1/auth/config", nil, &out)
}

func (c *Client) Me(ctx context.Context) (*api.Me, error) {
	var out api.Me
	return &out, c.do(ctx, http.MethodGet, "/api/v1/me", nil, &out)
}

func (c *Client) ListArtifacts(ctx context.Context, query, status string) ([]api.Artifact, error) {
	v := url.Values{}
	if query != "" {
		v.Set("q", query)
	}
	if status != "" {
		v.Set("status", status)
	}
	p := "/api/v1/artifacts"
	if len(v) > 0 {
		p += "?" + v.Encode()
	}
	var out api.ArtifactList
	return out.Items, c.do(ctx, http.MethodGet, p, nil, &out)
}

func (c *Client) GetArtifact(ctx context.Context, id string) (*api.Artifact, error) {
	var out api.Artifact
	return &out, c.do(ctx, http.MethodGet, "/api/v1/artifacts/"+esc(id), nil, &out)
}

func (c *Client) CreateArtifact(ctx context.Context, req *api.CreateArtifactRequest) (*api.Artifact, error) {
	var out api.Artifact
	return &out, c.do(ctx, http.MethodPost, "/api/v1/artifacts", req, &out)
}

func (c *Client) CreateVersion(ctx context.Context, id string, req *api.PublishRequest) (*api.Artifact, error) {
	var out api.Artifact
	return &out, c.do(ctx, http.MethodPost, "/api/v1/artifacts/"+esc(id)+"/versions", req, &out)
}

func (c *Client) UpdateArtifact(ctx context.Context, id string, req *api.UpdateArtifactRequest) (*api.Artifact, error) {
	var out api.Artifact
	return &out, c.do(ctx, http.MethodPatch, "/api/v1/artifacts/"+esc(id), req, &out)
}

func (c *Client) DeleteArtifact(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/artifacts/"+esc(id), nil, nil)
}

func (c *Client) RotateArtifact(ctx context.Context, id string) (*api.Artifact, error) {
	var out api.Artifact
	return &out, c.do(ctx, http.MethodPost, "/api/v1/artifacts/"+esc(id)+"/rotate", nil, &out)
}

func (c *Client) ListVersions(ctx context.Context, id string) ([]api.Version, error) {
	var out api.VersionList
	return out.Items, c.do(ctx, http.MethodGet, "/api/v1/artifacts/"+esc(id)+"/versions", nil, &out)
}

func (c *Client) ActivateVersion(ctx context.Context, id string, n int) (*api.Artifact, error) {
	var out api.Artifact
	return &out, c.do(ctx, http.MethodPost, "/api/v1/artifacts/"+esc(id)+"/versions/"+strconv.Itoa(n)+"/activate", nil, &out)
}

func (c *Client) DeleteVersion(ctx context.Context, id string, n int) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/artifacts/"+esc(id)+"/versions/"+strconv.Itoa(n), nil, nil)
}

func (c *Client) ListFiles(ctx context.Context, id string, version int) (*api.FileList, error) {
	p := "/api/v1/artifacts/" + esc(id) + "/files"
	if version > 0 {
		p += "?version=" + strconv.Itoa(version)
	}
	var out api.FileList
	return &out, c.do(ctx, http.MethodGet, p, nil, &out)
}

func (c *Client) ListViews(ctx context.Context, id string) ([]api.View, error) {
	var out api.ViewList
	return out.Items, c.do(ctx, http.MethodGet, "/api/v1/artifacts/"+esc(id)+"/views", nil, &out)
}

// Progress receives upload progress.
type Progress func(sent, total int64)

// ErrSourceChanged reports that a source file changed while it was being
// read for packing or upload, so the bytes read are not one consistent
// version of it. Share and Update retry once; Watch waits for the tree to
// settle and tries again.
var ErrSourceChanged = errors.New("source changed while it was being read")

// UploadFile performs a resumable chunked upload of path and returns the
// upload ID and SHA-256 to pass to CreateArtifact/CreateVersion. The
// checksum is taken before the upload and checked against the bytes
// actually sent; a mismatch (the file changed mid-upload) returns an error
// wrapping ErrSourceChanged.
func (c *Client) UploadFile(ctx context.Context, path string, progress Progress) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", "", err
	}
	size := st.Size()
	h := sha256.New()
	if n, err := io.Copy(h, io.NewSectionReader(f, 0, size)); err != nil {
		return "", "", err
	} else if n != size {
		return "", "", fmt.Errorf("%w: %s shrank before upload", ErrSourceChanged, path)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	sent := &sentHash{h: sha256.New()}

	var up api.Upload
	if err := c.do(ctx, http.MethodPost, "/api/v1/uploads", &api.CreateUploadRequest{Size: size}, &up); err != nil {
		return "", "", err
	}
	chunk := up.ChunkSize
	if chunk <= 0 {
		chunk = 32 << 20
	}
	offset := int64(0)
	failures := 0
	for offset < size {
		n := chunk
		if size-offset < n {
			n = size - offset
		}
		if offset < sent.next {
			// Resuming below what was already hashed: the bytes from
			// offset on are sent again and may differ from those hashed.
			if err := sent.rewind(f, offset); err != nil {
				return "", "", fmt.Errorf("%s: %w", path, err)
			}
		}
		body := &sentReader{r: io.NewSectionReader(f, offset, n), left: n, sent: sent}
		req, err := c.newRequest(ctx, http.MethodPut, "/api/v1/uploads/"+esc(up.ID)+"?offset="+strconv.FormatInt(offset, 10), body)
		if err != nil {
			return "", "", err
		}
		req.ContentLength = n
		req.Header.Set("Content-Type", "application/octet-stream")
		var got api.Upload
		err = c.send(req, &got)
		if err == nil {
			offset = got.Received
			failures = 0
			if progress != nil {
				progress(offset, size)
			}
			continue
		}
		if errors.Is(err, ErrSourceChanged) {
			return "", "", fmt.Errorf("%s: %w", path, ErrSourceChanged)
		}
		if s := StatusOf(err); s != 0 && s != http.StatusConflict && s < 500 {
			return "", "", err
		}
		failures++
		if failures > 5 || ctx.Err() != nil {
			return "", "", err
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(time.Duration(failures*failures) * time.Second):
		}
		// Ask the server how much it has and resume from there.
		var cur api.Upload
		if gerr := c.do(ctx, http.MethodGet, "/api/v1/uploads/"+esc(up.ID), nil, &cur); gerr == nil {
			offset = cur.Received
		}
	}
	if hex.EncodeToString(sent.h.Sum(nil)) != sum {
		return "", "", fmt.Errorf("%w: %s was modified during upload", ErrSourceChanged, path)
	}
	return up.ID, sum, nil
}

// sentHash hashes the uploaded bytes in file order; next is the offset
// hashed up to. Every chunk starts at next: when a chunk is resumed below
// it, rewind re-hashes the file up to the resume offset, so the final
// comparison covers exactly the bytes the server holds and an edit in the
// re-sent range is caught here instead of by the server's checksum.
type sentHash struct {
	h    hash.Hash
	next int64
}

func (s *sentHash) rewind(f io.ReaderAt, offset int64) error {
	s.h.Reset()
	n, err := io.Copy(s.h, io.NewSectionReader(f, 0, offset))
	if err != nil {
		return err
	}
	if n != offset {
		return fmt.Errorf("%w: file shrank during upload", ErrSourceChanged)
	}
	s.next = offset
	return nil
}

// sentReader is one chunk's request body. It feeds sentHash and turns a
// file that ends before the chunk does into ErrSourceChanged, instead of
// a transport error that would be retried as a network failure.
type sentReader struct {
	r    io.Reader
	left int64
	sent *sentHash
}

func (s *sentReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.sent.h.Write(p[:n])
	s.sent.next += int64(n)
	s.left -= int64(n)
	if err == io.EOF && s.left > 0 {
		err = fmt.Errorf("%w: file shrank during upload", ErrSourceChanged)
	}
	return n, err
}

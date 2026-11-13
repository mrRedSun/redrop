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
		return e
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("unexpected response from server (is the URL a redrop server?): %w", err)
		}
	}
	return nil
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

// UploadFile performs a resumable chunked upload of path and returns the
// upload ID and SHA-256 to pass to CreateArtifact/CreateVersion.
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
	if _, err := io.Copy(h, f); err != nil {
		return "", "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))

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
		req, err := c.newRequest(ctx, http.MethodPut, "/api/v1/uploads/"+esc(up.ID)+"?offset="+strconv.FormatInt(offset, 10), io.NewSectionReader(f, offset, n))
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
	return up.ID, sum, nil
}

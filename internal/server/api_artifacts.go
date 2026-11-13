package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/internal/auth"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

const uploadTTL = 24 * time.Hour

// --- uploads ---

func uploadView(u *store.Upload, chunk int64) api.Upload {
	return api.Upload{ID: u.ID, Size: u.Size, Received: u.Received, ChunkSize: chunk, ExpiresAt: u.ExpiresAt}
}

func (s *Server) maxUploadBytes() int64 {
	// Compressed bundles plus archive overhead may slightly exceed content size.
	return s.cfg.MaxVersionBytes + s.cfg.MaxVersionBytes/10 + 10<<20
}

func (s *Server) createUpload(c fuego.ContextWithBody[api.CreateUploadRequest]) (api.Upload, error) {
	ctx := c.Context()
	p := principal(ctx)
	body, err := c.Body()
	if err != nil {
		return api.Upload{}, err
	}
	if body.Size > s.maxUploadBytes() {
		return api.Upload{}, errStatus(http.StatusRequestEntityTooLarge, fmt.Sprintf("upload exceeds the %s per-version limit", humanBytes(s.cfg.MaxVersionBytes)))
	}
	used, err := s.db.UsedBytes(ctx, p.User.ID)
	if err != nil {
		return api.Upload{}, fail(ctx, err)
	}
	if p.User.QuotaBytes > 0 && used+body.Size > p.User.QuotaBytes+s.cfg.MaxVersionBytes/10 {
		return api.Upload{}, errStatus(http.StatusRequestEntityTooLarge, fmt.Sprintf("storage quota exceeded: %s of %s used", humanBytes(used), humanBytes(p.User.QuotaBytes)))
	}
	u, err := s.db.CreateUpload(ctx, p.User.ID, body.Size, uploadTTL)
	if err != nil {
		return api.Upload{}, fail(ctx, err)
	}
	f, err := os.OpenFile(s.stagedPath(u.ID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return api.Upload{}, fail(ctx, err)
	}
	f.Close()
	return uploadView(u, s.cfg.ChunkSize), nil
}

func (s *Server) getUpload(c fuego.ContextNoBody) (api.Upload, error) {
	ctx := c.Context()
	u, err := s.db.UploadByID(ctx, principal(ctx).User.ID, c.PathParam("id"))
	if err != nil {
		return api.Upload{}, errNotFound("upload not found or expired")
	}
	return uploadView(u, s.cfg.ChunkSize), nil
}

func (s *Server) putChunk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principal(ctx)
	u, err := s.db.UploadByID(ctx, p.User.ID, r.PathValue("id"))
	if err != nil {
		writeErr(w, r, errNotFound("upload not found or expired"))
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeErr(w, r, errBadRequest("offset query parameter is required"))
		return
	}
	if offset != u.Received {
		w.Header().Set("Upload-Offset", strconv.FormatInt(u.Received, 10))
		writeErr(w, r, errConflict(fmt.Sprintf("offset mismatch: server has %d bytes; resume from there", u.Received)))
		return
	}
	f, err := os.OpenFile(s.stagedPath(u.ID), os.O_WRONLY, 0o600)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	defer f.Close()
	if err := f.Truncate(offset); err != nil {
		writeErr(w, r, err)
		return
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		writeErr(w, r, err)
		return
	}
	limit := s.cfg.ChunkSize
	if rem := u.Size - offset; rem < limit {
		limit = rem
	}
	n, err := io.Copy(f, io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeErr(w, r, errBadRequest("chunk transfer interrupted; resume from the reported offset"))
		return
	}
	if n > limit {
		writeErr(w, r, errStatus(http.StatusRequestEntityTooLarge, fmt.Sprintf("chunk too large: at most %d bytes at this offset", limit)))
		return
	}
	if err := f.Sync(); err != nil {
		writeErr(w, r, err)
		return
	}
	u.Received = offset + n
	if err := s.db.SetUploadReceived(ctx, u.ID, u.Received); err != nil {
		writeErr(w, r, err)
		return
	}
	fuego.SendJSON(w, r, uploadView(u, s.cfg.ChunkSize))
}

// --- artifacts ---

func (s *Server) artifactView(a *store.Artifact, includeOwner bool) api.Artifact {
	t := time.Now()
	out := api.Artifact{
		ID: a.ID, URL: s.cfg.ArtifactURL(a.Slug), Title: a.Title, Kind: a.Kind, Status: a.Status(t),
		Size: a.Size, TotalSize: a.TotalSize, Files: a.Files, Version: a.CurrentVersion, Versions: a.Versions,
		HasPassword: a.PasswordHash != "", MaxViews: a.MaxViews, Views: a.Views, Previews: a.Previews,
		LastViewedAt: a.LastViewedAt, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, ExpiresAt: a.ExpiresAt,
		TokenName: a.TokenName,
	}
	switch out.Status {
	case api.StatusExpired:
		p := a.ExpiresAt.Add(s.cfg.ExpiredRetention)
		out.PurgeAt = &p
	case api.StatusExhausted:
		if a.ExhaustedAt != nil {
			p := a.ExhaustedAt.Add(s.cfg.ViewWindowLimited)
			out.PurgeAt = &p
		}
	}
	if includeOwner {
		out.OwnerUsername = a.OwnerUsername
		out.URL = "" // admins see metadata, not links
	}
	return out
}

// ownArtifact loads an artifact owned by the caller.
func (s *Server) ownArtifact(ctx context.Context, id string) (*store.Artifact, error) {
	a, err := s.db.ArtifactByID(ctx, id)
	if err != nil || a.UserID != principal(ctx).User.ID {
		return nil, errNotFound("artifact not found")
	}
	return a, nil
}

// expiryFor resolves a TTL string into an expiry time.
func (s *Server) expiryFor(ttl string) (*time.Time, error) {
	var d time.Duration
	var never bool
	if strings.TrimSpace(ttl) == "" {
		d, never = s.cfg.DefaultTTL, s.cfg.DefaultNever
	} else {
		var err error
		if d, never, err = api.ParseTTL(ttl); err != nil {
			return nil, errBadRequest(err.Error())
		}
	}
	if never {
		if !s.cfg.AllowNever {
			return nil, errBadRequest("this server does not allow artifacts that never expire")
		}
		return nil, nil
	}
	if s.cfg.MaxTTL > 0 && d > s.cfg.MaxTTL {
		return nil, errBadRequest("ttl exceeds the server maximum of " + api.FormatTTL(s.cfg.MaxTTL))
	}
	t := time.Now().Add(d).UTC().Truncate(time.Second)
	return &t, nil
}

func validateArtifactPassword(pw string) error {
	if len(pw) > 256 {
		return errBadRequest("password is too long")
	}
	return nil
}

func (s *Server) listArtifacts(c fuego.ContextNoBody) (api.ArtifactList, error) {
	ctx := c.Context()
	return s.artifactList(ctx, store.ListFilter{UserID: principal(ctx).User.ID, Query: strings.TrimSpace(c.QueryParam("q"))}, c.QueryParam("status"), false)
}

func (s *Server) artifactList(ctx context.Context, f store.ListFilter, status string, admin bool) (api.ArtifactList, error) {
	list, err := s.db.ListArtifacts(ctx, f)
	if err != nil {
		return api.ArtifactList{}, fail(ctx, err)
	}
	out := api.ArtifactList{Items: []api.Artifact{}}
	for _, a := range list {
		v := s.artifactView(a, admin)
		if status != "" && status != "all" && v.Status != status {
			continue
		}
		out.Items = append(out.Items, v)
	}
	return out, nil
}

func (s *Server) createArtifact(c fuego.ContextWithBody[api.CreateArtifactRequest]) (api.Artifact, error) {
	ctx := c.Context()
	p := principal(ctx)
	body, err := c.Body()
	if err != nil {
		return api.Artifact{}, err
	}
	exp, err := s.expiryFor(body.TTL)
	if err != nil {
		return api.Artifact{}, err
	}
	if body.MaxViews < 0 || body.MaxViews > 1_000_000 {
		return api.Artifact{}, errBadRequest("max_views must be between 0 and 1000000")
	}
	if err := validateArtifactPassword(body.Password); err != nil {
		return api.Artifact{}, err
	}
	var pwHash string
	if body.Password != "" {
		if pwHash, err = auth.HashPassword(body.Password); err != nil {
			return api.Artifact{}, fail(ctx, err)
		}
	}
	v, title, err := s.materialize(ctx, p.User.ID, &body.PublishRequest)
	if err != nil {
		return api.Artifact{}, err
	}
	if t := strings.TrimSpace(body.Title); t != "" {
		title = truncate(t, 200)
	}
	na := &store.NewArtifact{UserID: p.User.ID, Title: title, PasswordHash: pwHash, MaxViews: body.MaxViews, ExpiresAt: exp}
	if p.Token != nil {
		na.TokenID, na.TokenName = p.Token.ID, p.Token.Name
		v.TokenName = p.Token.Name
	}
	id, err := s.db.CreateArtifact(ctx, na, v, s.cfg.MaxTotalBytes)
	if err != nil {
		return api.Artifact{}, s.quotaErr(ctx, err)
	}
	a, err := s.db.ArtifactByID(ctx, id)
	if err != nil {
		return api.Artifact{}, fail(ctx, err)
	}
	s.log.Info("artifact created", "id", id, "user", p.User.Username, "kind", v.Kind, "bytes", a.Size, "token", na.TokenName)
	return s.artifactView(a, false), nil
}

func (s *Server) quotaErr(ctx context.Context, err error) error {
	if errors.Is(err, store.ErrQuota) {
		return errStatus(http.StatusRequestEntityTooLarge, "storage quota exceeded (your quota or the server total); delete or let old artifacts expire")
	}
	return fail(ctx, err)
}

func (s *Server) getArtifact(c fuego.ContextNoBody) (api.Artifact, error) {
	a, err := s.ownArtifact(c.Context(), c.PathParam("id"))
	if err != nil {
		return api.Artifact{}, err
	}
	return s.artifactView(a, false), nil
}

func (s *Server) updateArtifact(c fuego.ContextWithBody[api.UpdateArtifactRequest]) (api.Artifact, error) {
	ctx := c.Context()
	a, err := s.ownArtifact(ctx, c.PathParam("id"))
	if err != nil {
		return api.Artifact{}, err
	}
	body, err := c.Body()
	if err != nil {
		return api.Artifact{}, err
	}
	var u store.ArtifactUpdate
	if body.Title != nil {
		t := truncate(strings.TrimSpace(*body.Title), 200)
		u.Title = &t
	}
	if body.TTL != nil {
		if strings.TrimSpace(*body.TTL) == "" {
			return api.Artifact{}, errBadRequest("ttl must not be empty")
		}
		exp, err := s.expiryFor(*body.TTL)
		if err != nil {
			return api.Artifact{}, err
		}
		u.ExpiresAt = &exp
	}
	if body.Password != nil {
		if err := validateArtifactPassword(*body.Password); err != nil {
			return api.Artifact{}, err
		}
		h := ""
		if *body.Password != "" {
			if h, err = auth.HashPassword(*body.Password); err != nil {
				return api.Artifact{}, fail(ctx, err)
			}
		}
		u.PasswordHash = &h
	}
	if body.MaxViews != nil {
		if *body.MaxViews < 0 || *body.MaxViews > 1_000_000 {
			return api.Artifact{}, errBadRequest("max_views must be between 0 and 1000000")
		}
		u.MaxViews = body.MaxViews
	}
	u.ResetViews = body.ResetViews
	u.Revoked = body.Revoked
	u.SPA = body.SPA
	if err := s.db.UpdateArtifact(ctx, a.ID, u); err != nil {
		return api.Artifact{}, fail(ctx, err)
	}
	if a, err = s.db.ArtifactByID(ctx, a.ID); err != nil {
		return api.Artifact{}, fail(ctx, err)
	}
	return s.artifactView(a, false), nil
}

func (s *Server) deleteArtifact(c fuego.ContextNoBody) (api.OK, error) {
	ctx := c.Context()
	a, err := s.ownArtifact(ctx, c.PathParam("id"))
	if err != nil {
		return api.OK{}, err
	}
	if err := s.db.DeleteArtifact(ctx, a.ID); err != nil {
		return api.OK{}, fail(ctx, err)
	}
	s.log.Info("artifact deleted", "id", a.ID, "user", principal(ctx).User.Username)
	return api.OK{OK: true}, nil
}

func (s *Server) rotateArtifact(c fuego.ContextNoBody) (api.Artifact, error) {
	ctx := c.Context()
	a, err := s.ownArtifact(ctx, c.PathParam("id"))
	if err != nil {
		return api.Artifact{}, err
	}
	if err := s.db.UpdateArtifact(ctx, a.ID, store.ArtifactUpdate{RotateSlug: true}); err != nil {
		return api.Artifact{}, fail(ctx, err)
	}
	return s.getArtifact(c)
}

func (s *Server) previewArtifact(c fuego.ContextNoBody) (api.PreviewLink, error) {
	ctx := c.Context()
	a, err := s.ownArtifact(ctx, c.PathParam("id"))
	if err != nil {
		return api.PreviewLink{}, err
	}
	exp := time.Now().Add(5 * time.Minute)
	tok := s.previewSigner.Sign(a.ID+"|"+strconv.Itoa(a.AccessGen), exp)
	return api.PreviewLink{URL: s.cfg.ArtifactURL(a.Slug) + "__redrop/preview?t=" + tok, ExpiresAt: exp.UTC()}, nil
}

func (s *Server) listVersions(c fuego.ContextNoBody) (api.VersionList, error) {
	ctx := c.Context()
	a, err := s.ownArtifact(ctx, c.PathParam("id"))
	if err != nil {
		return api.VersionList{}, err
	}
	list, err := s.db.ListVersions(ctx, a.ID)
	if err != nil {
		return api.VersionList{}, fail(ctx, err)
	}
	out := api.VersionList{Items: []api.Version{}}
	for _, v := range list {
		out.Items = append(out.Items, api.Version{Number: v.Number, Kind: v.Kind, Size: v.Size, Files: v.Files, Entry: v.Entry, SPA: v.SPA,
			Note: v.Note, CreatedAt: v.CreatedAt, TokenName: v.TokenName, Current: v.Number == a.CurrentVersion})
	}
	return out, nil
}

func (s *Server) createVersion(c fuego.ContextWithBody[api.PublishRequest]) (api.Artifact, error) {
	ctx := c.Context()
	p := principal(ctx)
	a, err := s.ownArtifact(ctx, c.PathParam("id"))
	if err != nil {
		return api.Artifact{}, err
	}
	body, err := c.Body()
	if err != nil {
		return api.Artifact{}, err
	}
	v, _, err := s.materialize(ctx, p.User.ID, &body)
	if err != nil {
		return api.Artifact{}, err
	}
	if p.Token != nil {
		v.TokenName = p.Token.Name
	}
	n, err := s.db.AddVersion(ctx, a.ID, p.User.ID, v, s.cfg.MaxTotalBytes, s.cfg.KeepVersions)
	if err != nil {
		return api.Artifact{}, s.quotaErr(ctx, err)
	}
	s.log.Info("artifact version published", "id", a.ID, "version", n, "user", p.User.Username)
	a, err = s.db.ArtifactByID(ctx, a.ID)
	if err != nil {
		return api.Artifact{}, fail(ctx, err)
	}
	return s.artifactView(a, false), nil
}

func (s *Server) versionParam(c fuego.ContextNoBody) (*store.Artifact, int, error) {
	a, err := s.ownArtifact(c.Context(), c.PathParam("id"))
	if err != nil {
		return nil, 0, err
	}
	n, err := strconv.Atoi(c.PathParam("number"))
	if err != nil || n < 1 {
		return nil, 0, errBadRequest("invalid version number")
	}
	return a, n, nil
}

func (s *Server) activateVersion(c fuego.ContextNoBody) (api.Artifact, error) {
	a, n, err := s.versionParam(c)
	if err != nil {
		return api.Artifact{}, err
	}
	if err := s.db.SetCurrentVersion(c.Context(), a.ID, n); err != nil {
		return api.Artifact{}, fail(c.Context(), err)
	}
	return s.getArtifact(c)
}

func (s *Server) deleteVersion(c fuego.ContextNoBody) (api.OK, error) {
	a, n, err := s.versionParam(c)
	if err != nil {
		return api.OK{}, err
	}
	if n == a.CurrentVersion {
		return api.OK{}, errConflict("cannot delete the current version; activate another first")
	}
	return api.OK{OK: true}, fail(c.Context(), s.db.DeleteVersion(c.Context(), a.ID, n))
}

func (s *Server) listFiles(c fuego.ContextNoBody) (api.FileList, error) {
	ctx := c.Context()
	a, err := s.ownArtifact(ctx, c.PathParam("id"))
	if err != nil {
		return api.FileList{}, err
	}
	v := a.CurrentVersion
	if q := c.QueryParam("version"); q != "" {
		if v, err = strconv.Atoi(q); err != nil {
			return api.FileList{}, errBadRequest("invalid version")
		}
	}
	files, err := s.db.ListFiles(ctx, a.ID, v)
	if err != nil {
		return api.FileList{}, fail(ctx, err)
	}
	out := api.FileList{Version: v, Items: []api.FileEntry{}}
	for _, f := range files {
		out.Items = append(out.Items, api.FileEntry{Path: f.Path, Size: f.Size, ContentType: f.ContentType})
	}
	return out, nil
}

func (s *Server) listViews(c fuego.ContextNoBody) (api.ViewList, error) {
	ctx := c.Context()
	a, err := s.ownArtifact(ctx, c.PathParam("id"))
	if err != nil {
		return api.ViewList{}, err
	}
	rows, err := s.db.ListViews(ctx, a.ID, 200)
	if err != nil {
		return api.ViewList{}, fail(ctx, err)
	}
	out := api.ViewList{Items: []api.View{}}
	for _, v := range rows {
		out.Items = append(out.Items, api.View{At: v.At, Outcome: v.Outcome, Country: v.Country, Client: v.Client})
	}
	return out, nil
}

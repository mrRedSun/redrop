package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/pkg/api"
)

// --- uploads ---

func (a *API) createUpload(c fuego.ContextWithBody[api.CreateUploadRequest]) (api.Upload, error) {
	body, err := c.Body()
	if err != nil {
		return api.Upload{}, err
	}
	u, err := a.sharing.CreateUpload(c.Context(), actorFrom(c.Context()), body.Size)
	if err != nil {
		return api.Upload{}, err
	}
	return uploadDTO(u), nil
}

func (a *API) getUpload(c fuego.ContextNoBody) (api.Upload, error) {
	u, err := a.sharing.GetUpload(c.Context(), actorFrom(c.Context()), c.PathParam("id"))
	if err != nil {
		return api.Upload{}, err
	}
	return uploadDTO(u), nil
}

// putChunk streams a raw body, so it is a plain net/http handler.
func (a *API) putChunk(w http.ResponseWriter, r *http.Request) {
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeError(w, r, apperr.Invalid("offset query parameter is required"))
		return
	}
	u, err := a.sharing.AppendChunk(r.Context(), actorFrom(r.Context()), r.PathValue("id"), offset, r.Body)
	if err != nil {
		var mismatch *sharing.OffsetMismatchError
		if errors.As(err, &mismatch) {
			w.Header().Set("Upload-Offset", strconv.FormatInt(mismatch.Received, 10))
		}
		writeError(w, r, err)
		return
	}
	fuego.SendJSON(w, r, uploadDTO(u))
}

// --- artifacts ---

func (a *API) listArtifacts(c fuego.ContextNoBody) (api.ArtifactList, error) {
	list, err := a.sharing.List(c.Context(), actorFrom(c.Context()), c.QueryParam("q"), c.QueryParam("status"))
	if err != nil {
		return api.ArtifactList{}, err
	}
	return a.artifactList(list, false), nil
}

func (a *API) createArtifact(c fuego.ContextWithBody[api.CreateArtifactRequest]) (api.Artifact, error) {
	body, err := c.Body()
	if err != nil {
		return api.Artifact{}, err
	}
	m, err := a.sharing.Create(c.Context(), actorFrom(c.Context()), contentDTO(body.PublishRequest),
		sharing.Settings{Title: body.Title, TTL: body.TTL, Password: body.Password, MaxViews: body.MaxViews})
	if err != nil {
		return api.Artifact{}, err
	}
	return a.artifactDTO(m, false), nil
}

func (a *API) getArtifact(c fuego.ContextNoBody) (api.Artifact, error) {
	m, err := a.sharing.Get(c.Context(), actorFrom(c.Context()), c.PathParam("id"))
	if err != nil {
		return api.Artifact{}, err
	}
	return a.artifactDTO(m, false), nil
}

func (a *API) updateArtifact(c fuego.ContextWithBody[api.UpdateArtifactRequest]) (api.Artifact, error) {
	body, err := c.Body()
	if err != nil {
		return api.Artifact{}, err
	}
	m, err := a.sharing.Update(c.Context(), actorFrom(c.Context()), c.PathParam("id"), sharing.Patch{
		Title: body.Title, TTL: body.TTL, Password: body.Password, MaxViews: body.MaxViews,
		ResetViews: body.ResetViews, Revoked: body.Revoked, SPA: body.SPA,
	})
	if err != nil {
		return api.Artifact{}, err
	}
	return a.artifactDTO(m, false), nil
}

func (a *API) deleteArtifact(c fuego.ContextNoBody) (api.OK, error) {
	err := a.sharing.Delete(c.Context(), actorFrom(c.Context()), c.PathParam("id"))
	return api.OK{OK: err == nil}, err
}

func (a *API) rotateArtifact(c fuego.ContextNoBody) (api.Artifact, error) {
	m, err := a.sharing.Rotate(c.Context(), actorFrom(c.Context()), c.PathParam("id"))
	if err != nil {
		return api.Artifact{}, err
	}
	return a.artifactDTO(m, false), nil
}

func (a *API) previewArtifact(c fuego.ContextNoBody) (api.PreviewLink, error) {
	p, err := a.sharing.Preview(c.Context(), actorFrom(c.Context()), c.PathParam("id"))
	if err != nil {
		return api.PreviewLink{}, err
	}
	return api.PreviewLink{URL: p.URL, ExpiresAt: p.ExpiresAt}, nil
}

func (a *API) listVersions(c fuego.ContextNoBody) (api.VersionList, error) {
	m, rows, err := a.sharing.Versions(c.Context(), actorFrom(c.Context()), c.PathParam("id"))
	if err != nil {
		return api.VersionList{}, err
	}
	return versionList(m.CurrentVersion, rows), nil
}

func (a *API) createVersion(c fuego.ContextWithBody[api.PublishRequest]) (api.Artifact, error) {
	body, err := c.Body()
	if err != nil {
		return api.Artifact{}, err
	}
	m, err := a.sharing.AddVersion(c.Context(), actorFrom(c.Context()), c.PathParam("id"), contentDTO(body))
	if err != nil {
		return api.Artifact{}, err
	}
	return a.artifactDTO(m, false), nil
}

func versionNumber(c fuego.ContextNoBody) (int, error) {
	n, err := strconv.Atoi(c.PathParam("number"))
	if err != nil || n < 1 {
		return 0, apperr.Invalid("invalid version number")
	}
	return n, nil
}

func (a *API) activateVersion(c fuego.ContextNoBody) (api.Artifact, error) {
	n, err := versionNumber(c)
	if err != nil {
		return api.Artifact{}, err
	}
	m, err := a.sharing.ActivateVersion(c.Context(), actorFrom(c.Context()), c.PathParam("id"), n)
	if err != nil {
		return api.Artifact{}, err
	}
	return a.artifactDTO(m, false), nil
}

func (a *API) deleteVersion(c fuego.ContextNoBody) (api.OK, error) {
	n, err := versionNumber(c)
	if err != nil {
		return api.OK{}, err
	}
	err = a.sharing.DeleteVersion(c.Context(), actorFrom(c.Context()), c.PathParam("id"), n)
	return api.OK{OK: err == nil}, err
}

func (a *API) listFiles(c fuego.ContextNoBody) (api.FileList, error) {
	version := 0
	if q := c.QueryParam("version"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil {
			return api.FileList{}, apperr.Invalid("invalid version")
		}
		version = n
	}
	v, files, err := a.sharing.Files(c.Context(), actorFrom(c.Context()), c.PathParam("id"), version)
	if err != nil {
		return api.FileList{}, err
	}
	return fileList(v, files), nil
}

func (a *API) listViews(c fuego.ContextNoBody) (api.ViewList, error) {
	rows, err := a.sharing.Views(c.Context(), actorFrom(c.Context()), c.PathParam("id"))
	if err != nil {
		return api.ViewList{}, err
	}
	return viewList(rows), nil
}

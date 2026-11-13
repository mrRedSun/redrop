package httpapi

import (
	"time"

	"github.com/mrRedSun/redrop/internal/accounts"
	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// Mapping from service/store models to wire types lives here so handlers
// stay free of field-by-field conversions.

func (a *API) artifactDTO(m *store.Artifact, adminView bool) api.Artifact {
	status := m.Status(time.Now())
	out := api.Artifact{
		ID: m.ID, URL: a.sharing.URL(m), Title: m.Title, Kind: m.Kind, Status: status,
		Size: m.Size, TotalSize: m.TotalSize, Files: m.Files, Version: m.CurrentVersion, Versions: m.Versions,
		HasPassword: m.PasswordHash != "", MaxViews: m.MaxViews, Views: m.Views, Previews: m.Previews,
		LastViewedAt: m.LastViewedAt, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, ExpiresAt: m.ExpiresAt,
		PurgeAt: a.sharing.PurgeAt(m, status), TokenName: m.TokenName,
	}
	if adminView {
		// Admins see metadata only, never the secret link.
		out.OwnerUsername, out.URL = m.OwnerUsername, ""
	}
	return out
}

func (a *API) artifactList(list []*store.Artifact, adminView bool) api.ArtifactList {
	out := api.ArtifactList{Items: make([]api.Artifact, 0, len(list))}
	for _, m := range list {
		out.Items = append(out.Items, a.artifactDTO(m, adminView))
	}
	return out
}

func versionList(current int, rows []store.VersionRow) api.VersionList {
	out := api.VersionList{Items: make([]api.Version, 0, len(rows))}
	for _, v := range rows {
		out.Items = append(out.Items, api.Version{Number: v.Number, Kind: v.Kind, Size: v.Size, Files: v.Files, Entry: v.Entry, SPA: v.SPA,
			Note: v.Note, CreatedAt: v.CreatedAt, TokenName: v.TokenName, Current: v.Number == current})
	}
	return out
}

func fileList(version int, files []store.File) api.FileList {
	out := api.FileList{Version: version, Items: make([]api.FileEntry, 0, len(files))}
	for _, f := range files {
		out.Items = append(out.Items, api.FileEntry{Path: f.Path, Size: f.Size, ContentType: f.ContentType})
	}
	return out
}

func viewList(rows []store.ViewRow) api.ViewList {
	out := api.ViewList{Items: make([]api.View, 0, len(rows))}
	for _, v := range rows {
		out.Items = append(out.Items, api.View{At: v.At, Outcome: v.Outcome, Country: v.Country, Client: v.Client})
	}
	return out
}

func uploadDTO(u *sharing.Upload) api.Upload {
	return api.Upload{ID: u.ID, Size: u.Size, Received: u.Received, ChunkSize: u.ChunkSize, ExpiresAt: u.ExpiresAt}
}

func userDTO(u *store.User, usage accounts.Usage) api.User {
	return api.User{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role,
		QuotaBytes: u.QuotaBytes, UsedBytes: usage.UsedBytes, TOTPEnabled: u.TOTPEnabled,
		Disabled: u.Disabled, CreatedAt: u.CreatedAt, Artifacts: usage.Artifacts,
	}
}

func tokenDTO(t store.Token) api.Token {
	return api.Token{ID: t.ID, Name: t.Name, Prefix: t.Prefix, CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt, ExpiresAt: t.ExpiresAt, Artifacts: t.Artifacts}
}

func inviteDTO(o *store.OneTime) api.Invite {
	return api.Invite{ID: o.ID, Note: o.Note, Role: o.Role, QuotaBytes: o.QuotaBytes, CreatedAt: o.CreatedAt, ExpiresAt: o.ExpiresAt, UsedAt: o.UsedAt, UsedBy: o.UsedByName}
}

func providersDTO(ps []accounts.Provider) []api.AuthProvider {
	out := make([]api.AuthProvider, 0, len(ps))
	for _, p := range ps {
		out = append(out, api.AuthProvider{ID: p.ID, Label: p.Label})
	}
	return out
}

func contentDTO(p api.PublishRequest) sharing.Content {
	return sharing.Content{UploadID: p.UploadID, SHA256: p.SHA256, Kind: p.Kind, Filename: p.Filename, Entry: p.Entry, SPA: p.SPA, Note: p.Note}
}

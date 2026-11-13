package httpapi

import (
	"github.com/mrRedSun/redrop/internal/accounts"
	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/pkg/api"
)

// Pure mappings between service results and wire types. No policy lives
// here: status, links, and privacy are decided by the services, and this file
// must not import internal/store (see imports_test.go).

func artifactMetaDTO(m sharing.ArtifactMeta) api.Artifact {
	return api.Artifact{
		ID: m.ID, Title: m.Title, Kind: m.Kind, Status: m.Status,
		Size: m.Size, TotalSize: m.TotalSize, Files: m.Files, Version: m.Version, Versions: m.Versions,
		HasPassword: m.HasPassword, MaxViews: m.MaxViews, Views: m.Views, Previews: m.Previews,
		LastViewedAt: m.LastViewedAt, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, ExpiresAt: m.ExpiresAt,
		PurgeAt: m.PurgeAt, TokenName: m.TokenName,
	}
}

func artifactDTO(a sharing.Artifact) api.Artifact {
	out := artifactMetaDTO(a.ArtifactMeta)
	out.URL = a.URL
	return out
}

// adminArtifactDTO has no link to copy: sharing.AdminArtifact carries none.
func adminArtifactDTO(a sharing.AdminArtifact) api.Artifact {
	out := artifactMetaDTO(a.ArtifactMeta)
	out.OwnerUsername = a.OwnerUsername
	return out
}

func mapItems[T, U any](in []T, f func(T) U) []U {
	out := make([]U, 0, len(in))
	for _, v := range in {
		out = append(out, f(v))
	}
	return out
}

func versionDTO(v sharing.Version) api.Version {
	return api.Version{Number: v.Number, Kind: v.Kind, Size: v.Size, Files: v.Files, Entry: v.Entry, SPA: v.SPA,
		Note: v.Note, CreatedAt: v.CreatedAt, TokenName: v.TokenName, Current: v.Current}
}

func fileListDTO(f sharing.FileList) api.FileList {
	return api.FileList{Version: f.Version, Items: mapItems(f.Items, func(e sharing.FileEntry) api.FileEntry {
		return api.FileEntry{Path: e.Path, Size: e.Size, ContentType: e.ContentType}
	})}
}

func viewDTO(v sharing.View) api.View {
	return api.View{At: v.At, Outcome: v.Outcome, Country: v.Country, Client: v.Client}
}

func uploadDTO(u *sharing.Upload) api.Upload {
	return api.Upload{ID: u.ID, Size: u.Size, Received: u.Received, ChunkSize: u.ChunkSize, ExpiresAt: u.ExpiresAt}
}

func statsDTO(s sharing.Stats) api.Stats {
	return api.Stats{Users: s.Users, Artifacts: s.Artifacts, ActiveArtifacts: s.Active, LogicalBytes: s.LogicalBytes,
		StoredBytes: s.StoredBytes, MaxTotalBytes: s.MaxTotalBytes, MaxVersionBytes: s.MaxVersionBytes}
}

func userDTO(u accounts.User) api.User {
	return api.User{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role,
		QuotaBytes: u.QuotaBytes, UsedBytes: u.UsedBytes, TOTPEnabled: u.TOTPEnabled,
		Disabled: u.Disabled, CreatedAt: u.CreatedAt, Artifacts: u.Artifacts,
	}
}

func meDTO(m accounts.Me) api.Me {
	return api.Me{User: userDTO(m.User), AuthMethod: m.AuthMethod, TokenName: m.TokenName, CanAdmin: m.CanAdmin}
}

func tokenDTO(t accounts.Token) api.Token {
	return api.Token{ID: t.ID, Name: t.Name, Prefix: t.Prefix, CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt, ExpiresAt: t.ExpiresAt, Artifacts: t.Artifacts}
}

func identityDTO(i accounts.Identity) api.Identity {
	return api.Identity{ID: i.ID, Provider: i.Provider, Label: i.Label, Email: i.Email, CreatedAt: i.CreatedAt}
}

func inviteDTO(o accounts.Invite) api.Invite {
	return api.Invite{ID: o.ID, Note: o.Note, Role: o.Role, QuotaBytes: o.QuotaBytes, CreatedAt: o.CreatedAt, ExpiresAt: o.ExpiresAt, UsedAt: o.UsedAt, UsedBy: o.UsedBy}
}

func providersDTO(ps []accounts.Provider) []api.AuthProvider {
	return mapItems(ps, func(p accounts.Provider) api.AuthProvider { return api.AuthProvider{ID: p.ID, Label: p.Label} })
}

// --- requests ---

func contentFromRequest(p api.PublishRequest) sharing.Content {
	return sharing.Content{UploadID: p.UploadID, SHA256: p.SHA256, Kind: p.Kind, Filename: p.Filename, Entry: p.Entry, SPA: p.SPA, Note: p.Note}
}

func settingsFromRequest(r api.CreateArtifactRequest) sharing.Settings {
	return sharing.Settings{Title: r.Title, TTL: r.TTL, Password: r.Password, MaxViews: r.MaxViews}
}

func patchFromRequest(r api.UpdateArtifactRequest) sharing.Patch {
	return sharing.Patch{Title: r.Title, TTL: r.TTL, Password: r.Password, MaxViews: r.MaxViews, ResetViews: r.ResetViews, Revoked: r.Revoked, SPA: r.SPA}
}

func userPatchFromRequest(r api.AdminUpdateUserRequest) accounts.UserPatch {
	return accounts.UserPatch{QuotaBytes: r.QuotaBytes, Disabled: r.Disabled, Role: r.Role, DisplayName: r.DisplayName}
}

func inviteFromRequest(r api.CreateInviteRequest) accounts.InviteRequest {
	return accounts.InviteRequest{Note: r.Note, Role: r.Role, QuotaBytes: r.QuotaBytes, TTL: r.TTL}
}

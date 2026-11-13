package server

import (
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-fuego/fuego"
	"github.com/go-fuego/fuego/option"
)

func (s *Server) registerRoutes(f *fuego.Server) {
	any := []func(*fuego.BaseRoute){
		option.Middleware(s.authMW(accessAny)),
		option.Security(openapi3.SecurityRequirement{"bearer": {}}, openapi3.SecurityRequirement{"session": {}}),
	}
	sess := []func(*fuego.BaseRoute){
		option.Middleware(s.authMW(accessSession)),
		option.Security(openapi3.SecurityRequirement{"session": {}}),
	}
	admin := []func(*fuego.BaseRoute){
		option.Middleware(s.authMW(accessAdmin)),
		option.Security(openapi3.SecurityRequirement{"session": {}}),
	}
	public := []func(*fuego.BaseRoute){option.Middleware(s.csrfOnly)}

	v1 := fuego.Group(f, "/api/v1")

	// Auth
	authG := fuego.Group(v1, "", option.Tags("auth"))
	fuego.Get(authG, "/auth/config", s.getAuthConfig, option.Summary("Login options and server info"), option.OperationID("getAuthConfig"))
	fuego.Post(authG, "/auth/login", s.login, append(public, option.Summary("Sign in with username and password"), option.OperationID("login"))...)
	fuego.Post(authG, "/auth/logout", s.logout, append(public, option.Summary("Sign out"), option.OperationID("logout"))...)
	fuego.GetStd(authG, "/auth/oidc/{provider}/start", s.oidcStart, option.Summary("Begin single sign-on (browser redirect)"), option.OperationID("oidcStart"),
		option.Query("mode", "login, link, or invite"), option.Query("token", "Invite token when mode=invite"))
	fuego.GetStd(authG, "/auth/oidc/{provider}/callback", s.oidcCallback, option.Summary("Single sign-on callback"), option.OperationID("oidcCallback"), option.Hide())
	fuego.Post(authG, "/setup", s.setup, append(public, option.Summary("Create the first admin account"), option.OperationID("setup"))...)
	fuego.Get(authG, "/invites/{token}", s.inviteInfo, option.Summary("Inspect an invite link"), option.OperationID("getInvite"))
	fuego.Post(authG, "/invites/{token}/accept", s.acceptInvite, append(public, option.Summary("Create an account from an invite"), option.OperationID("acceptInvite"))...)
	fuego.Get(authG, "/reset/{token}", s.resetInfo, option.Summary("Inspect a password reset link"), option.OperationID("getReset"))
	fuego.Post(authG, "/reset/{token}", s.resetPassword, append(public, option.Summary("Set a new password from a reset link"), option.OperationID("resetPassword"))...)

	// Account
	me := fuego.Group(v1, "/me", option.Tags("account"))
	fuego.Get(me, "", s.getMe, append(any, option.Summary("Current user"), option.OperationID("getMe"))...)
	fuego.Patch(me, "", s.updateMe, append(sess, option.Summary("Update profile"), option.OperationID("updateMe"))...)
	fuego.Post(me, "/password", s.changePassword, append(sess, option.Summary("Change password"), option.OperationID("changePassword"))...)
	fuego.Post(me, "/totp/setup", s.totpSetup, append(sess, option.Summary("Start TOTP enrollment"), option.OperationID("totpSetup"))...)
	fuego.Post(me, "/totp/enable", s.totpEnable, append(sess, option.Summary("Confirm TOTP enrollment"), option.OperationID("totpEnable"))...)
	fuego.Post(me, "/totp/disable", s.totpDisable, append(sess, option.Summary("Disable TOTP"), option.OperationID("totpDisable"))...)
	fuego.Get(me, "/identities", s.listIdentities, append(sess, option.Summary("Linked single sign-on identities"), option.OperationID("listIdentities"))...)
	fuego.Delete(me, "/identities/{id}", s.unlinkIdentity, append(sess, option.Summary("Unlink an identity"), option.OperationID("unlinkIdentity"))...)

	// Tokens
	tok := fuego.Group(v1, "/tokens", option.Tags("tokens"))
	fuego.Get(tok, "", s.listTokens, append(sess, option.Summary("List API tokens"), option.OperationID("listTokens"))...)
	fuego.Post(tok, "", s.createToken, append(sess, option.Summary("Create an API token"), option.OperationID("createToken"), option.DefaultStatusCode(http.StatusCreated))...)
	fuego.Delete(tok, "/{id}", s.deleteToken, append(sess, option.Summary("Revoke an API token"), option.OperationID("deleteToken"))...)

	// Uploads
	up := fuego.Group(v1, "/uploads", option.Tags("uploads"))
	fuego.Post(up, "", s.createUpload, append(any, option.Summary("Start a chunked upload"), option.OperationID("createUpload"), option.DefaultStatusCode(http.StatusCreated))...)
	fuego.Get(up, "/{id}", s.getUpload, append(any, option.Summary("Upload progress (for resuming)"), option.OperationID("getUpload"))...)
	fuego.PutStd(up, "/{id}", s.putChunk, append(any, option.Summary("Append a chunk at ?offset=N (raw body)"), option.OperationID("putChunk"),
		option.Query("offset", "Byte offset of this chunk; must equal bytes received so far", fuego.ParamRequired()),
		option.RequestContentType("application/octet-stream"))...)

	// Artifacts
	art := fuego.Group(v1, "/artifacts", option.Tags("artifacts"))
	fuego.Get(art, "", s.listArtifacts, append(any, option.Summary("List your artifacts"), option.OperationID("listArtifacts"),
		option.Query("q", "Search title, id, or token name"), option.Query("status", "active, expired, exhausted, revoked, or all (default all)"))...)
	fuego.Post(art, "", s.createArtifact, append(any, option.Summary("Publish an upload as a new artifact"), option.OperationID("createArtifact"), option.DefaultStatusCode(http.StatusCreated))...)
	fuego.Get(art, "/{id}", s.getArtifact, append(any, option.Summary("Get an artifact"), option.OperationID("getArtifact"))...)
	fuego.Patch(art, "/{id}", s.updateArtifact, append(any, option.Summary("Change title, expiry, password, view limit, or revoke"), option.OperationID("updateArtifact"))...)
	fuego.Delete(art, "/{id}", s.deleteArtifact, append(any, option.Summary("Delete an artifact and all versions"), option.OperationID("deleteArtifact"))...)
	fuego.Post(art, "/{id}/rotate", s.rotateArtifact, append(any, option.Summary("Replace the link; the old URL stops working"), option.OperationID("rotateArtifact"))...)
	fuego.Post(art, "/{id}/preview", s.previewArtifact, append(sess, option.Summary("Short-lived owner preview link (not counted as a view)"), option.OperationID("previewArtifact"))...)
	fuego.Get(art, "/{id}/versions", s.listVersions, append(any, option.Summary("List versions"), option.OperationID("listVersions"))...)
	fuego.Post(art, "/{id}/versions", s.createVersion, append(any, option.Summary("Publish an upload as a new version (same link)"), option.OperationID("createVersion"), option.DefaultStatusCode(http.StatusCreated))...)
	fuego.Post(art, "/{id}/versions/{number}/activate", s.activateVersion, append(any, option.Summary("Roll back or forward to a version"), option.OperationID("activateVersion"))...)
	fuego.Delete(art, "/{id}/versions/{number}", s.deleteVersion, append(any, option.Summary("Delete a non-current version"), option.OperationID("deleteVersion"))...)
	fuego.Get(art, "/{id}/files", s.listFiles, append(any, option.Summary("Files in a version"), option.OperationID("listFiles"), option.QueryInt("version", "Version number (default current)"))...)
	fuego.Get(art, "/{id}/views", s.listViews, append(any, option.Summary("Recent access log"), option.OperationID("listViews"))...)

	// Admin
	adm := fuego.Group(v1, "/admin", option.Tags("admin"))
	fuego.Get(adm, "/stats", s.adminStats, append(admin, option.Summary("Server usage"), option.OperationID("adminStats"))...)
	fuego.Get(adm, "/users", s.adminListUsers, append(admin, option.Summary("List users"), option.OperationID("adminListUsers"))...)
	fuego.Patch(adm, "/users/{id}", s.adminUpdateUser, append(admin, option.Summary("Change quota, role, or disable a user"), option.OperationID("adminUpdateUser"))...)
	fuego.Post(adm, "/users/{id}/reset", s.adminResetLink, append(admin, option.Summary("Create a password reset link"), option.OperationID("adminResetLink"))...)
	fuego.Get(adm, "/invites", s.adminListInvites, append(admin, option.Summary("List invites"), option.OperationID("adminListInvites"))...)
	fuego.Post(adm, "/invites", s.adminCreateInvite, append(admin, option.Summary("Create an invite link"), option.OperationID("adminCreateInvite"), option.DefaultStatusCode(http.StatusCreated))...)
	fuego.Delete(adm, "/invites/{id}", s.adminDeleteInvite, append(admin, option.Summary("Revoke an invite"), option.OperationID("adminDeleteInvite"))...)
	fuego.Get(adm, "/artifacts", s.adminListArtifacts, append(admin, option.Summary("All artifacts (metadata only)"), option.OperationID("adminListArtifacts"), option.Query("q", "Search"))...)
	fuego.Patch(adm, "/artifacts/{id}", s.adminUpdateArtifact, append(admin, option.Summary("Revoke or restore any artifact"), option.OperationID("adminUpdateArtifact"))...)
	fuego.Delete(adm, "/artifacts/{id}", s.adminDeleteArtifact, append(admin, option.Summary("Delete any artifact"), option.OperationID("adminDeleteArtifact"))...)

	// Non-API routes on the app origin.
	fuego.GetStd(f, "/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok\n"))
	}, option.Hide())
	fuego.GetStd(f, "/robots.txt", robotsTxt, option.Hide())
	fuego.GetStd(f, "/install.sh", s.installSh, option.Hide())
	fuego.GetStd(f, "/install.ps1", s.installPs1, option.Hide())
	fuego.GetStd(f, "/download/{file}", s.downloadCLI, option.Hide())
	fuego.GetStd(f, "/", s.spa, option.Hide())
}

func robotsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("User-agent: *\nDisallow: /\n"))
}

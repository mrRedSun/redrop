package httpapi

import (
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-fuego/fuego"
	"github.com/go-fuego/fuego/option"
)

type routeOpts = []func(*fuego.BaseRoute)

func with(base routeOpts, extra ...func(*fuego.BaseRoute)) routeOpts {
	return append(append(routeOpts{}, base...), extra...)
}

func op(id, summary string) []func(*fuego.BaseRoute) {
	return []func(*fuego.BaseRoute){option.OperationID(id), option.Summary(summary)}
}

func (a *API) routes() {
	s := a.server
	bearerOrSession := []openapi3.SecurityRequirement{{"bearer": {}}, {"session": {}}}
	sessionSec := openapi3.SecurityRequirement{"session": {}}

	anyAuthOpts := routeOpts{option.Middleware(a.requireAuth(anyAuth)), option.Security(bearerOrSession...)}
	sessionOpts := routeOpts{option.Middleware(a.requireAuth(sessionOnly)), option.Security(sessionSec)}
	adminOpts := routeOpts{option.Middleware(a.requireAuth(adminOnly)), option.Security(sessionSec)}
	publicWrite := routeOpts{option.Middleware(a.csrfOnly)}

	v1 := fuego.Group(s, "/api/v1")

	// Authentication and one-time links.
	authG := fuego.Group(v1, "", option.Tags("auth"))
	fuego.Get(authG, "/auth/config", a.getAuthConfig, op("getAuthConfig", "Login options and server info")...)
	fuego.Post(authG, "/auth/login", a.login, with(publicWrite, op("login", "Sign in with username and password")...)...)
	fuego.Post(authG, "/auth/logout", a.logout, with(publicWrite, op("logout", "Sign out")...)...)
	fuego.GetStd(authG, "/auth/oidc/{provider}/start", a.ssoStart, with(nil, append(op("ssoStart", "Begin single sign-on (browser redirect)"),
		option.Query("mode", "login, link, or invite"), option.Query("token", "Invite token when mode=invite"))...)...)
	fuego.GetStd(authG, "/auth/oidc/{provider}/callback", a.ssoCallback, option.Hide())
	fuego.Post(authG, "/setup", a.setup, with(publicWrite, op("setup", "Create the first admin account")...)...)
	fuego.Get(authG, "/invites/{token}", a.inviteInfo, op("getInvite", "Inspect an invite link")...)
	fuego.Post(authG, "/invites/{token}/accept", a.acceptInvite, with(publicWrite, op("acceptInvite", "Create an account from an invite")...)...)
	fuego.Get(authG, "/reset/{token}", a.resetInfo, op("getReset", "Inspect a password reset link")...)
	fuego.Post(authG, "/reset/{token}", a.resetPassword, with(publicWrite, op("resetPassword", "Set a new password from a reset link")...)...)

	// Account.
	me := fuego.Group(v1, "/me", option.Tags("account"))
	fuego.Get(me, "", a.getMe, with(anyAuthOpts, op("getMe", "Current user")...)...)
	fuego.Patch(me, "", a.updateMe, with(sessionOpts, op("updateMe", "Update profile")...)...)
	fuego.Post(me, "/password", a.changePassword, with(sessionOpts, op("changePassword", "Change password")...)...)
	fuego.Post(me, "/totp/setup", a.totpSetup, with(sessionOpts, op("totpSetup", "Start authenticator enrollment")...)...)
	fuego.Post(me, "/totp/enable", a.totpEnable, with(sessionOpts, op("totpEnable", "Confirm authenticator enrollment")...)...)
	fuego.Post(me, "/totp/disable", a.totpDisable, with(sessionOpts, op("totpDisable", "Disable the authenticator")...)...)
	fuego.Get(me, "/identities", a.listIdentities, with(sessionOpts, op("listIdentities", "Linked single sign-on identities")...)...)
	fuego.Delete(me, "/identities/{id}", a.unlinkIdentity, with(sessionOpts, op("unlinkIdentity", "Unlink an identity")...)...)

	// API tokens.
	tok := fuego.Group(v1, "/tokens", option.Tags("tokens"))
	fuego.Get(tok, "", a.listTokens, with(sessionOpts, op("listTokens", "List API tokens")...)...)
	fuego.Post(tok, "", a.createToken, with(sessionOpts, append(op("createToken", "Create an API token"), option.DefaultStatusCode(http.StatusCreated))...)...)
	fuego.Delete(tok, "/{id}", a.deleteToken, with(sessionOpts, op("deleteToken", "Revoke an API token")...)...)

	// Uploads.
	up := fuego.Group(v1, "/uploads", option.Tags("uploads"))
	fuego.Post(up, "", a.createUpload, with(anyAuthOpts, append(op("createUpload", "Start a chunked upload"), option.DefaultStatusCode(http.StatusCreated))...)...)
	fuego.Get(up, "/{id}", a.getUpload, with(anyAuthOpts, op("getUpload", "Upload progress (for resuming)")...)...)
	fuego.PutStd(up, "/{id}", a.putChunk, with(anyAuthOpts, append(op("putChunk", "Append a chunk at ?offset=N (raw body)"),
		option.Query("offset", "Byte offset of this chunk; must equal bytes received so far", fuego.ParamRequired()),
		option.RequestContentType("application/octet-stream"))...)...)

	// Artifacts.
	art := fuego.Group(v1, "/artifacts", option.Tags("artifacts"))
	fuego.Get(art, "", a.listArtifacts, with(anyAuthOpts, append(op("listArtifacts", "List your artifacts"),
		option.Query("q", "Search title, id, or token name"), option.Query("status", "active, expired, exhausted, revoked, or all"))...)...)
	fuego.Post(art, "", a.createArtifact, with(anyAuthOpts, append(op("createArtifact", "Publish an upload as a new artifact"), option.DefaultStatusCode(http.StatusCreated))...)...)
	fuego.Get(art, "/{id}", a.getArtifact, with(anyAuthOpts, op("getArtifact", "Get an artifact")...)...)
	fuego.Patch(art, "/{id}", a.updateArtifact, with(anyAuthOpts, op("updateArtifact", "Change title, expiry, password, view limit, or revoke")...)...)
	fuego.Delete(art, "/{id}", a.deleteArtifact, with(anyAuthOpts, op("deleteArtifact", "Delete an artifact and all versions")...)...)
	fuego.Post(art, "/{id}/rotate", a.rotateArtifact, with(anyAuthOpts, op("rotateArtifact", "Replace the link; the old URL stops working")...)...)
	fuego.Post(art, "/{id}/preview", a.previewArtifact, with(sessionOpts, op("previewArtifact", "Short-lived owner preview link (not counted)")...)...)
	fuego.Get(art, "/{id}/versions", a.listVersions, with(anyAuthOpts, op("listVersions", "List versions")...)...)
	fuego.Post(art, "/{id}/versions", a.createVersion, with(anyAuthOpts, append(op("createVersion", "Publish an upload as a new version (same link)"), option.DefaultStatusCode(http.StatusCreated))...)...)
	fuego.Post(art, "/{id}/versions/{number}/activate", a.activateVersion, with(anyAuthOpts, op("activateVersion", "Make a version current")...)...)
	fuego.Delete(art, "/{id}/versions/{number}", a.deleteVersion, with(anyAuthOpts, op("deleteVersion", "Delete a non-current version")...)...)
	fuego.Get(art, "/{id}/files", a.listFiles, with(anyAuthOpts, append(op("listFiles", "Files in a version"), option.QueryInt("version", "Version number (default current)"))...)...)
	fuego.Get(art, "/{id}/views", a.listViews, with(anyAuthOpts, op("listViews", "Recent access log")...)...)

	// Administration.
	adm := fuego.Group(v1, "/admin", option.Tags("admin"))
	fuego.Get(adm, "/stats", a.admin.stats, with(adminOpts, op("adminStats", "Server usage")...)...)
	fuego.Get(adm, "/users", a.admin.listUsers, with(adminOpts, op("adminListUsers", "List users")...)...)
	fuego.Patch(adm, "/users/{id}", a.admin.updateUser, with(adminOpts, op("adminUpdateUser", "Change quota, role, or disable a user")...)...)
	fuego.Post(adm, "/users/{id}/reset", a.admin.resetLink, with(adminOpts, op("adminResetLink", "Create a password reset link")...)...)
	fuego.Get(adm, "/invites", a.admin.listInvites, with(adminOpts, op("adminListInvites", "List invites")...)...)
	fuego.Post(adm, "/invites", a.admin.createInvite, with(adminOpts, append(op("adminCreateInvite", "Create an invite link"), option.DefaultStatusCode(http.StatusCreated))...)...)
	fuego.Delete(adm, "/invites/{id}", a.admin.deleteInvite, with(adminOpts, op("adminDeleteInvite", "Revoke an invite")...)...)
	fuego.Get(adm, "/artifacts", a.admin.listArtifacts, with(adminOpts, append(op("adminListArtifacts", "All artifacts (metadata only)"),
		option.Query("q", "Search"), option.Query("status", "active, expired, exhausted, revoked, or all"))...)...)
	fuego.Patch(adm, "/artifacts/{id}", a.admin.updateArtifact, with(adminOpts, op("adminUpdateArtifact", "Revoke or restore any artifact")...)...)
	fuego.Delete(adm, "/artifacts/{id}", a.admin.deleteArtifact, with(adminOpts, op("adminDeleteArtifact", "Delete any artifact")...)...)

	fuego.GetStd(s, "/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok\n"))
	}, option.Hide())
	fuego.GetStd(v1, "/{path...}", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, errNoRoute)
	}, option.Hide())
}

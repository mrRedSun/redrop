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
	jsonOnly := option.RequestContentType("application/json")
	publicWrite := routeOpts{option.Middleware(a.csrfOnly), option.Middleware(a.authBody), jsonOnly}
	// Routes that decode a JSON body add jsonIn after authentication, so a
	// body is read only from an authorized caller.
	anyAuthJSON := with(anyAuthOpts, option.Middleware(a.jsonIn), jsonOnly)
	sessionJSON := with(sessionOpts, option.Middleware(a.jsonIn), jsonOnly)
	adminJSON := with(adminOpts, option.Middleware(a.jsonIn), jsonOnly)

	v1 := fuego.Group(s, "/api/v1")

	// Authentication and one-time links.
	authG := fuego.Group(v1, "", option.Tags("auth"))
	fuego.Get(authG, "/auth/config", a.getAuthConfig, op("getAuthConfig", "Login options and server info")...)
	fuego.Get(authG, "/version", a.getVersion, op("getVersion", "Server build version and minimum supported CLI version")...)
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
	fuego.Patch(me, "", a.updateMe, with(sessionJSON, op("updateMe", "Update profile")...)...)
	fuego.Post(me, "/password", a.changePassword, with(sessionJSON, op("changePassword", "Change password")...)...)
	fuego.Post(me, "/totp/setup", a.totpSetup, with(sessionOpts, op("totpSetup", "Start authenticator enrollment")...)...)
	fuego.Post(me, "/totp/enable", a.totpEnable, with(sessionJSON, op("totpEnable", "Confirm authenticator enrollment")...)...)
	fuego.Post(me, "/totp/disable", a.totpDisable, with(sessionJSON, op("totpDisable", "Disable the authenticator")...)...)
	fuego.Get(me, "/identities", a.listIdentities, with(sessionOpts, op("listIdentities", "Linked single sign-on identities")...)...)
	fuego.Delete(me, "/identities/{id}", a.unlinkIdentity, with(sessionOpts, op("unlinkIdentity", "Unlink an identity")...)...)

	// API tokens.
	tok := fuego.Group(v1, "/tokens", option.Tags("tokens"))
	fuego.Get(tok, "", a.listTokens, with(sessionOpts, op("listTokens", "List API tokens")...)...)
	fuego.Post(tok, "", a.createToken, with(sessionJSON, append(op("createToken", "Create an API token"), option.DefaultStatusCode(http.StatusCreated))...)...)
	fuego.Delete(tok, "/{id}", a.deleteToken, with(sessionOpts, op("deleteToken", "Revoke an API token")...)...)

	// Uploads.
	up := fuego.Group(v1, "/uploads", option.Tags("uploads"))
	fuego.Post(up, "", a.createUpload, with(anyAuthJSON, append(op("createUpload", "Start a chunked upload"), option.DefaultStatusCode(http.StatusCreated))...)...)
	fuego.Get(up, "/{id}", a.getUpload, with(anyAuthOpts, op("getUpload", "Upload progress (for resuming)")...)...)
	fuego.PutStd(up, "/{id}", a.putChunk, with(anyAuthOpts, append(op("putChunk", "Append a chunk at ?offset=N (raw body)"),
		option.Query("offset", "Byte offset of this chunk; must equal bytes received so far", fuego.ParamRequired()),
		option.RequestContentType("application/octet-stream"))...)...)

	// Artifacts.
	art := fuego.Group(v1, "/artifacts", option.Tags("artifacts"))
	fuego.Get(art, "", a.listArtifacts, with(anyAuthOpts, append(op("listArtifacts", "List your artifacts"),
		option.Query("q", "Search title, id, or token name"), option.Query("status", "active, expired, exhausted, revoked, or all"))...)...)
	fuego.Post(art, "", a.createArtifact, with(anyAuthJSON, append(op("createArtifact", "Publish an upload as a new artifact"), option.DefaultStatusCode(http.StatusCreated))...)...)
	fuego.Post(art, "/reservations", a.reserveArtifact, with(anyAuthOpts, append(op("reserveArtifact", "Reserve an artifact ID for an end-to-end encrypted share"), option.DefaultStatusCode(http.StatusCreated))...)...)
	fuego.Get(art, "/{id}", a.getArtifact, with(anyAuthOpts, op("getArtifact", "Get an artifact")...)...)
	fuego.Patch(art, "/{id}", a.updateArtifact, with(anyAuthJSON, op("updateArtifact", "Change title, expiry, password, view limit, or revoke")...)...)
	fuego.Delete(art, "/{id}", a.deleteArtifact, with(anyAuthOpts, op("deleteArtifact", "Delete an artifact and all versions")...)...)
	fuego.Post(art, "/{id}/rotate", a.rotateArtifact, with(anyAuthOpts, op("rotateArtifact", "Replace the link; the old URL stops working")...)...)
	fuego.Post(art, "/{id}/preview", a.previewArtifact, with(sessionOpts, op("previewArtifact", "Short-lived owner preview link (not counted)")...)...)
	fuego.Get(art, "/{id}/versions", a.listVersions, with(anyAuthOpts, op("listVersions", "List versions")...)...)
	fuego.Post(art, "/{id}/versions", a.createVersion, with(anyAuthJSON, append(op("createVersion", "Publish an upload as a new version (same link)"), option.DefaultStatusCode(http.StatusCreated))...)...)
	fuego.Post(art, "/{id}/versions/{number}/activate", a.activateVersion, with(anyAuthOpts, op("activateVersion", "Make a version current")...)...)
	fuego.Delete(art, "/{id}/versions/{number}", a.deleteVersion, with(anyAuthOpts, op("deleteVersion", "Delete a non-current version")...)...)
	fuego.Get(art, "/{id}/files", a.listFiles, with(anyAuthOpts, append(op("listFiles", "Files in a version"), option.QueryInt("version", "Version number (default current)"))...)...)
	fuego.Get(art, "/{id}/views", a.listViews, with(anyAuthOpts, op("listViews", "Recent access log")...)...)
	fuego.Get(art, "/{id}/feedback", a.listFeedback, with(anyAuthOpts, append(op("listFeedback", "Viewer comments, oldest first (untrusted text)"),
		option.Query("status", "open or all (default all)"), option.QueryInt("version", "Only comments on this version"))...)...)
	fuego.Patch(art, "/{id}/feedback/{comment}", a.resolveComment, with(anyAuthJSON, op("resolveComment", "Mark a comment resolved or open again")...)...)
	fuego.Delete(art, "/{id}/feedback/{comment}", a.deleteComment, with(anyAuthOpts, op("deleteComment", "Delete a comment")...)...)
	fuego.Put(art, "/{id}/live/{session}", a.holdLive, with(anyAuthOpts, op("holdLive", "Start or renew a watch session's live reload lease")...)...)
	fuego.Delete(art, "/{id}/live/{session}", a.releaseLive, with(anyAuthOpts, op("releaseLive", "End a watch session's live reload lease")...)...)

	// Administration.
	adm := fuego.Group(v1, "/admin", option.Tags("admin"))
	fuego.Get(adm, "/stats", a.admin.stats, with(adminOpts, op("adminStats", "Server usage")...)...)
	fuego.Get(adm, "/users", a.admin.listUsers, with(adminOpts, op("adminListUsers", "List users")...)...)
	fuego.Patch(adm, "/users/{id}", a.admin.updateUser, with(adminJSON, op("adminUpdateUser", "Change quota, role, or disable a user")...)...)
	fuego.Post(adm, "/users/{id}/reset", a.admin.resetLink, with(adminJSON, op("adminResetLink", "Create a password reset link")...)...)
	fuego.Get(adm, "/invites", a.admin.listInvites, with(adminOpts, op("adminListInvites", "List invites")...)...)
	fuego.Post(adm, "/invites", a.admin.createInvite, with(adminJSON, append(op("adminCreateInvite", "Create an invite link"), option.DefaultStatusCode(http.StatusCreated))...)...)
	fuego.Delete(adm, "/invites/{id}", a.admin.deleteInvite, with(adminOpts, op("adminDeleteInvite", "Revoke an invite")...)...)
	fuego.Get(adm, "/artifacts", a.admin.listArtifacts, with(adminOpts, append(op("adminListArtifacts", "All artifacts (metadata only)"),
		option.Query("q", "Search"), option.Query("status", "active, expired, exhausted, revoked, or all"))...)...)
	fuego.Patch(adm, "/artifacts/{id}", a.admin.updateArtifact, with(adminJSON, op("adminUpdateArtifact", "Revoke or restore any artifact")...)...)
	fuego.Delete(adm, "/artifacts/{id}", a.admin.deleteArtifact, with(adminOpts, op("adminDeleteArtifact", "Delete any artifact")...)...)

	// Abuse reports (DESIGN.md "Abuse handling"): the public form's endpoint
	// and the admin queue.
	rep := fuego.Group(v1, "/reports", option.Tags("reports"))
	fuego.Post(rep, "", a.submitReport, append(op("submitReport", "Report an abusive shared link (no account; same-origin only)"),
		option.Middleware(a.sameOriginOnly), option.Middleware(a.reportBody), jsonOnly, option.DefaultStatusCode(http.StatusAccepted))...)
	fuego.Get(adm, "/reports", a.admin.listReports, with(adminOpts, append(op("adminListReports", "Abuse report queue, newest first"),
		option.Query("status", "open (default), resolved, dismissed, or all"))...)...)
	fuego.Patch(adm, "/reports/{id}", a.admin.updateReport, with(adminJSON, op("adminUpdateReport", "Resolve, dismiss, or reopen a report")...)...)
	fuego.Post(adm, "/reports/{id}/revoke", a.admin.revokeReported, with(adminOpts, op("adminRevokeReported", "Revoke the reported artifact as an administrator and resolve its reports")...)...)
	fuego.Post(adm, "/reports/{id}/suspend", a.admin.suspendReportedOwner, with(adminOpts, op("adminSuspendReportedOwner", "Suspend the reported artifact's owner: disable the account and revoke all of its artifacts")...)...)

	fuego.GetStd(s, "/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok\n"))
	}, option.Hide())
	// Unknown /api/ paths, including /api/docs while the docs page is off,
	// are JSON 404s rather than the dashboard.
	fuego.GetStd(s, "/api/{path...}", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, errNoRoute)
	}, option.Hide())
}

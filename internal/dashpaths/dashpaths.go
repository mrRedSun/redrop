// Package dashpaths lists the dashboard (SPA) routes the server links or
// redirects to. They are a contract with web/src/app/paths.ts; a test keeps
// the two in sync.
package dashpaths

const (
	Home          = "/"
	Login         = "/login"
	Account       = "/account"
	AccountLinked = "/account?linked=1"
	TokensWelcome = "/tokens?welcome=1"
	// Report is the public abuse report form. Links from the content
	// domain add the page as "#url=<page>"; a fragment never reaches a
	// server or a proxy log.
	Report       = "/report"
	AdminReports = "/admin/reports"

	// One-time link prefixes; the secret token follows the slash.
	Setup  = "/setup/"
	Invite = "/invite/"
	Reset  = "/reset/"
)

// SecretPrefixes are path prefixes (dashboard and API) whose next segment is a
// secret that must never be logged: one-time tokens, and the slug in an
// end-to-end encrypted link's intake page (api.E2EIntakePrefix).
var SecretPrefixes = []string{"/api/v1/invites/", "/api/v1/reset/", Invite, Reset, Setup, "/e/"}

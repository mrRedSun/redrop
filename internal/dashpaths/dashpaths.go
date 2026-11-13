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

	// One-time link prefixes; the secret token follows the slash.
	Setup  = "/setup/"
	Invite = "/invite/"
	Reset  = "/reset/"
)

// SecretPrefixes are path prefixes (dashboard and API) whose next segment is a
// one-time secret that must never be logged.
var SecretPrefixes = []string{"/api/v1/invites/", "/api/v1/reset/", Invite, Reset, Setup}

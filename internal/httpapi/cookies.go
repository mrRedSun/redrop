package httpapi

import (
	"net/http"
	"time"

	"github.com/mrRedSun/redrop/internal/accounts"
)

// cookies names and writes dashboard cookies. In production they use the
// __Host- prefix (Secure, host-only, Path=/); plain-HTTP development drops it.
type cookies struct{ insecure bool }

func (c cookies) name(base string) string {
	if c.insecure {
		return "redrop_" + base
	}
	return "__Host-redrop_" + base
}

func (c cookies) sessionName() string { return c.name("session") }
func (c cookies) ssoName() string     { return c.name("sso") }

func (c cookies) set(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: !c.insecure, SameSite: http.SameSiteLaxMode,
	})
}

func (c cookies) setSession(w http.ResponseWriter, s *accounts.Session) {
	c.set(w, c.sessionName(), s.Secret, int(time.Until(s.ExpiresAt).Seconds()))
}

func (c cookies) clearSession(w http.ResponseWriter) { c.set(w, c.sessionName(), "", -1) }

func (c cookies) setSSO(w http.ResponseWriter, state string) {
	c.set(w, c.ssoName(), state, int(accounts.SSOStateTTL.Seconds()))
}

func (c cookies) clearSSO(w http.ResponseWriter) { c.set(w, c.ssoName(), "", -1) }

func cookieValue(r *http.Request, name string) string {
	if c, err := r.Cookie(name); err == nil {
		return c.Value
	}
	return ""
}

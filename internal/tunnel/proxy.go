package tunnel

import (
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync/atomic"
)

// Visit is a visitor's request that passed the access check (the content
// handler's job), with what the proxy needs to know about it.
type Visit struct {
	ArtifactID string
	// AccessGen is the access generation the request was let in under; a
	// password change or rotation closes its streams.
	AccessGen int
	// ClientIP is the address the server resolved for its rate limits; it
	// becomes X-Forwarded-For.
	ClientIP string
	// Proto is the content domain's scheme, for X-Forwarded-Proto.
	Proto string
	// Reserved reports a cookie name that belongs to redrop on content
	// hosts. Such cookies never reach the app, and the app can't set them.
	Reserved func(name string) bool
	// Strip names more request headers to remove: the configured client-IP
	// and country headers.
	Strip []string
	// Page writes redrop's own message page.
	Page func(w http.ResponseWriter, status int, heading, body string)
}

// Serve forwards req through the tunnel and writes the app's answer. It
// returns false, having written nothing, when the tunnel is not connected.
// The caller has already set the content host's own response headers on w.
func (r *Registry) Serve(w http.ResponseWriter, req *http.Request, v Visit) bool {
	l := r.connected(v.ArtifactID)
	if l == nil {
		return false
	}
	if req.ContentLength > r.lim.MaxRequestBytes {
		v.Page(w, http.StatusRequestEntityTooLarge, "Too large", "This request is larger than the server allows for a tunnel.")
		return true
	}
	if r.budget.Blocked(v.ArtifactID) {
		r.overBudget(w, v)
		return true
	}
	release, err := l.acquire(req.Context())
	switch {
	case errors.Is(err, errOffline):
		return false
	case errors.Is(err, errBusy):
		w.Header().Set("Retry-After", "5")
		v.Page(w, http.StatusServiceUnavailable, "This tunnel is busy", "It is answering as many requests as it may at once. Try again in a moment.")
		return true
	case err != nil:
		return true // the visitor went away
	}
	defer release()
	p := &proxy{reg: r, l: l, v: v, w: w}
	rp := &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		Transport:      l.transport,
		FlushInterval:  -1, // stream: dev servers send events and chunks
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.fail,
		ErrorLog:       discardLog,
	}
	rp.ServeHTTP(w, req)
	return true
}

func (r *Registry) overBudget(w http.ResponseWriter, v Visit) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(r.budget.Wait(v.ArtifactID).Seconds()))))
	v.Page(w, http.StatusTooManyRequests, "This tunnel used its traffic for now", "It has moved as much data this hour as the server allows. Try again later.")
}

var (
	errRequestTooLarge  = errors.New("request body over the tunnel limit")
	errResponseTooLarge = errors.New("response body over the tunnel limit")
)

// proxy is one request's proxying state.
type proxy struct {
	reg     *Registry
	l       *link
	v       Visit
	w       http.ResponseWriter
	reqBody *limitedBody
}

// rewrite prepares the outbound request (DESIGN.md "What the app
// receives"). ReverseProxy has already removed the hop-by-hop headers
// (keeping Upgrade for upgrades) and every Forwarded and X-Forwarded-*
// header the visitor sent.
func (p *proxy) rewrite(pr *httputil.ProxyRequest) {
	out := pr.Out
	out.URL.Scheme = "http"
	out.URL.Host = pr.In.Host
	out.Host = pr.In.Host
	cleanRequestHeader(out.Header, p.v.Strip, p.v.Reserved)
	out.Header.Set("X-Forwarded-For", p.v.ClientIP)
	out.Header.Set("X-Forwarded-Host", pr.In.Host)
	out.Header.Set("X-Forwarded-Proto", p.v.Proto)
	if out.Body != nil && out.Body != http.NoBody {
		p.reqBody = &limitedBody{ReadCloser: out.Body, left: p.reg.lim.MaxRequestBytes, err: errRequestTooLarge}
		out.Body = p.reqBody
	}
	pr.Out = out.WithContext(withGen(out.Context(), p.v.AccessGen))
}

// proxyRemoved are request headers that describe the visitor's path to
// redrop, not the visitor's request to the app.
var proxyRemoved = []string{"X-Real-Ip", "True-Client-Ip", "Cdn-Loop"}

// cleanRequestHeader removes proxy and CDN headers (every CF-* header among
// them) and redrop's own cookies.
func cleanRequestHeader(h http.Header, strip []string, reserved func(string) bool) {
	for _, k := range append(proxyRemoved, strip...) {
		h.Del(k)
	}
	for k := range h {
		if strings.HasPrefix(k, "Cf-") {
			delete(h, k)
		}
	}
	var kept []string
	for _, line := range h.Values("Cookie") {
		for _, pair := range strings.Split(line, ";") {
			pair = strings.TrimSpace(pair)
			name, _, _ := strings.Cut(pair, "=")
			if pair != "" && !reserved(strings.TrimSpace(name)) {
				kept = append(kept, pair)
			}
		}
	}
	h.Del("Cookie")
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}

// modifyResponse applies the response rules (DESIGN.md "What visitors
// receive") and bounds the body.
func (p *proxy) modifyResponse(resp *http.Response) error {
	cleanResponseHeader(p.w.Header(), resp.Header, p.v.Reserved)
	if resp.StatusCode == http.StatusSwitchingProtocols || resp.Request.Method == http.MethodHead {
		return nil
	}
	max := p.reg.lim.MaxResponseBytes
	if resp.ContentLength > max {
		return errResponseTooLarge
	}
	if resp.Body != nil && resp.Body != http.NoBody {
		resp.Body = &limitedBody{ReadCloser: resp.Body, left: max, err: errResponseTooLarge}
	}
	return nil
}

// forcedHeaders are set by the content handler for every content response
// and are not the app's to choose; the app's own values are dropped.
var forcedHeaders = []string{"X-Robots-Tag", "Referrer-Policy", "X-Content-Type-Options", "Strict-Transport-Security"}

// cacheHeaders could let a CDN keep a response and serve it without the
// access check; the content handler's private Cache-Control stands instead.
var cacheHeaders = []string{"Cache-Control", "Expires", "Cdn-Cache-Control", "Cloudflare-Cdn-Cache-Control", "Surrogate-Control"}

// cleanResponseHeader adjusts the app's response headers (app) and the
// headers the content handler already set (own) before they are merged.
func cleanResponseHeader(own, app http.Header, reserved func(string) bool) {
	for _, k := range forcedHeaders {
		app.Del(k)
	}
	noStore := false
	for _, v := range app.Values("Cache-Control") {
		if strings.Contains(strings.ToLower(v), "no-store") {
			noStore = true
		}
	}
	for _, k := range cacheHeaders {
		app.Del(k)
	}
	if noStore {
		own.Set("Cache-Control", "private, no-store")
	}
	// Artifacts' Permissions-Policy would switch camera, microphone and the
	// like off for an app under development: the app's own policy applies.
	own.Del("Permissions-Policy")
	if app.Get("Cross-Origin-Resource-Policy") != "" {
		own.Del("Cross-Origin-Resource-Policy")
	}
	cookies := app.Values("Set-Cookie")
	app.Del("Set-Cookie")
	for _, c := range cookies {
		if c, ok := hostOnlyCookie(c, reserved); ok {
			app.Add("Set-Cookie", c)
		}
	}
}

// hostOnlyCookie removes a Set-Cookie line's Domain attribute, so the cookie
// stays on the tunnel's own origin, and refuses a cookie named like one of
// redrop's. Everything else is kept byte for byte.
func hostOnlyCookie(line string, reserved func(string) bool) (string, bool) {
	parts := strings.Split(line, ";")
	name, _, _ := strings.Cut(parts[0], "=")
	if reserved(strings.TrimSpace(name)) {
		return "", false
	}
	kept := parts[:1]
	for _, attr := range parts[1:] {
		key, _, _ := strings.Cut(attr, "=")
		if strings.EqualFold(strings.TrimSpace(key), "domain") {
			continue
		}
		kept = append(kept, attr)
	}
	return strings.Join(kept, ";"), true
}

// fail answers a request the tunnel could not complete. Nothing about the
// request is logged but the tunnel and the kind of failure.
func (p *proxy) fail(w http.ResponseWriter, req *http.Request, err error) {
	kind := "no answer"
	var ne net.Error
	switch {
	case p.reqBody != nil && p.reqBody.over.Load():
		kind = "request too large"
		p.v.Page(w, http.StatusRequestEntityTooLarge, "Too large", "This request is larger than the server allows for a tunnel.")
	case errors.Is(err, errResponseTooLarge):
		kind = "response too large"
		p.v.Page(w, http.StatusBadGateway, "Answer too large", "The app's answer is larger than the server allows for a tunnel.")
	case p.reg.budget.Blocked(p.v.ArtifactID):
		kind = "over budget"
		p.reg.overBudget(w, p.v)
	case errors.Is(err, errOffline):
		kind = "offline"
		p.v.Page(w, http.StatusServiceUnavailable, "This tunnel is offline", offlineText)
	case errors.Is(err, errAccessChanged):
		kind = "access changed"
		p.v.Page(w, http.StatusServiceUnavailable, "Access changed", "The link's access changed. Reload the page.")
	case req.Context().Err() != nil:
		kind = "visitor left"
		w.WriteHeader(http.StatusBadGateway)
	case errors.As(err, &ne) && ne.Timeout():
		kind = "timeout"
		p.v.Page(w, http.StatusGatewayTimeout, "The app took too long", "The app behind this tunnel did not start answering in time.")
	default:
		p.v.Page(w, http.StatusBadGateway, "No answer", "The app behind this tunnel did not answer.")
	}
	p.reg.log.Debug("tunnel request failed", "id", p.v.ArtifactID, "kind", kind)
}

// offlineText is the body of the offline page.
const offlineText = "The app behind this link isn't connected right now. Try again later."

// OfflinePage writes the page visitors with access get while the tunnel is
// not connected.
func OfflinePage(w http.ResponseWriter, page func(http.ResponseWriter, int, string, string)) {
	w.Header().Set("Retry-After", "30")
	page(w, http.StatusServiceUnavailable, "This tunnel is offline", offlineText)
}

// limitedBody fails with err once more than left bytes were read.
type limitedBody struct {
	io.ReadCloser
	left int64
	err  error
	over atomic.Bool
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left < 0 {
		b.over.Store(true)
		return 0, b.err
	}
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		b.over.Store(true)
		return 0, b.err
	}
	return n, err
}

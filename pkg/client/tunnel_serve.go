package client

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

// tunnelKeepAlive is how often the CLI pings the server. Cloudflare drops a
// WebSocket after about 100 idle seconds; a ping is WebSocket data.
const tunnelKeepAlive = 30 * time.Second

// TunnelRequest describes one proxied request for --log: never headers,
// bodies or the query string.
type TunnelRequest struct {
	Method   string
	Path     string
	Status   int
	Duration time.Duration
}

// ServeTunnelSession serves one established tunnel connection: conn carries
// the yamux session the server opens streams on, and each stream is one
// HTTP/1.1 request for target. It returns when the session ends or ctx is
// done, closing conn. onRequest, if set, is called after each request.
func ServeTunnelSession(ctx context.Context, conn net.Conn, target *TunnelTarget, onRequest func(TunnelRequest)) error {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = tunnelKeepAlive
	cfg.LogOutput = io.Discard
	sess, err := yamux.Server(conn, cfg)
	if err != nil {
		conn.Close()
		return err
	}
	var h http.Handler = target.proxy()
	if onRequest != nil {
		h = logTunnelRequests(h, onRequest)
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	err = srv.Serve(sess)
	srv.Close()
	sess.Close()
	if ctx.Err() != nil || errors.Is(err, yamux.ErrSessionShutdown) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// proxy forwards a stream's request to the pinned target. Only the target's
// addresses are ever dialed: no proxy from the environment, no redirects
// followed.
func (t *TunnelTarget) proxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = t.Host
			pr.Out.Host = t.Host
			// The server set these from what it knows; Rewrite removed them.
			for _, k := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v := pr.In.Header.Values(k); len(v) > 0 {
					pr.Out.Header[k] = v
				}
			}
		},
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         t.dial,
			DisableCompression:  true,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
		FlushInterval:  -1,
		ModifyResponse: t.relativeLocation,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, "The app behind this tunnel did not answer.\n")
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
}

// dial connects to the first target address that answers.
func (t *TunnelTarget) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	var d net.Dialer
	var first error
	for _, a := range t.Addrs {
		c, err := d.DialContext(ctx, "tcp", a.String())
		if err == nil {
			return c, nil
		}
		if first == nil {
			first = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, first
}

// relativeLocation turns a redirect to the target's own host (an app that
// builds absolute URLs from Host) into a path on the tunnel's origin.
func (t *TunnelTarget) relativeLocation(resp *http.Response) error {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return nil
	}
	u, err := url.Parse(loc)
	if err != nil || !u.IsAbs() || !strings.EqualFold(u.Host, t.Host) {
		return nil
	}
	rel := "/" + strings.TrimLeft(u.EscapedPath(), `/\`)
	if u.RawQuery != "" {
		rel += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		rel += "#" + u.EscapedFragment()
	}
	resp.Header.Set("Location", rel)
	return nil
}

// logTunnelRequests reports each request once it has finished.
func logTunnelRequests(next http.Handler, report func(TunnelRequest)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		switch {
		case status == 0 && r.Header.Get("Upgrade") != "":
			status = http.StatusSwitchingProtocols // hijacked: the proxy wrote it
		case status == 0:
			status = http.StatusOK
		}
		report(TunnelRequest{Method: r.Method, Path: r.URL.Path, Status: status, Duration: time.Since(start)})
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets the proxy flush and hijack (WebSocket upgrades) through it.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

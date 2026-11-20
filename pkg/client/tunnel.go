package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/mrRedSun/redrop/pkg/api"
)

// Tunnel reconnect policy: jittered exponential backoff from 1 to 30
// seconds, reset after a connection that lasted a minute. A Retry-After is
// honoured up to MaxRetryAfter.
const (
	TunnelRetryBase = time.Second
	TunnelRetryMax  = 30 * time.Second
	tunnelStable    = time.Minute
	tunnelHandshake = 30 * time.Second
)

// CreateTunnel creates a tunnel artifact; RunTunnel connects it.
func (c *Client) CreateTunnel(ctx context.Context, req *api.CreateTunnelRequest) (*api.Artifact, error) {
	var a api.Artifact
	return &a, c.do(ctx, http.MethodPost, "/api/v1/tunnels", req, &a)
}

// TunnelEventKind labels what RunTunnel reports through OnEvent.
type TunnelEventKind string

const (
	// TunnelOnline: the tunnel is connected and serving.
	TunnelOnline TunnelEventKind = "online"
	// TunnelOffline: the connection ended; a reconnect follows after Delay.
	TunnelOffline TunnelEventKind = "offline"
	// TunnelRetry: connecting failed; the next attempt follows after Delay.
	TunnelRetry TunnelEventKind = "retry"
)

// TunnelEvent is one change of a running tunnel.
type TunnelEvent struct {
	Kind    TunnelEventKind
	Attempt int           // the upcoming attempt, for offline and retry
	Delay   time.Duration // the wait before it
	Err     error
}

// TunnelEndedError is returned when the server ended the tunnel for good
// (revoked, deleted, expired, ended by an administrator).
type TunnelEndedError struct{ Reason string }

func (e *TunnelEndedError) Error() string {
	if e.Reason == "" {
		return "the server ended the tunnel"
	}
	return "the server ended the tunnel: " + e.Reason
}

// TunnelOptions configures RunTunnel.
type TunnelOptions struct {
	ID     string // the tunnel artifact
	Target *TunnelTarget
	// OnEvent receives online, offline and retry events.
	OnEvent func(TunnelEvent)
	// OnRequest, if set, is called after each proxied request.
	OnRequest func(TunnelRequest)
	// Clock and Retrier override timekeeping and the backoff (tests).
	Clock   Clock
	Retrier Retrier
}

// RunTunnel connects the tunnel and serves it until ctx is done (returning
// nil), reconnecting with backoff after failures. It returns an error for
// failures retrying cannot fix: authentication, a missing or wrong
// artifact, the per-user limit, or a *TunnelEndedError.
func (c *Client) RunTunnel(ctx context.Context, o TunnelOptions) error {
	clock, retrier := o.Clock, o.Retrier
	if clock == nil {
		clock = realClock{}
	}
	if retrier == nil {
		retrier = ExponentialRetrier{Base: TunnelRetryBase, Max: TunnelRetryMax}
	}
	notify := func(e TunnelEvent) {
		if o.OnEvent != nil {
			o.OnEvent(e)
		}
	}
	wait := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-clock.After(d):
			return true
		}
	}
	session := newTunnelSession()
	failures := 0
	for {
		tc, err := c.dialTunnel(ctx, o.ID, session)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !retryableConnect(err) {
				return err
			}
			failures++
			delay := retrier.Next(failures)
			var apiErr *Error
			if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
				delay = max(delay, min(apiErr.RetryAfter, MaxRetryAfter))
			}
			notify(TunnelEvent{Kind: TunnelRetry, Attempt: failures + 1, Delay: delay, Err: err})
			if !wait(delay) {
				return nil
			}
			continue
		}
		notify(TunnelEvent{Kind: TunnelOnline})
		start := clock.Now()
		serveErr := ServeTunnelSession(ctx, tc, o.Target, o.OnRequest)
		if ctx.Err() != nil {
			return nil
		}
		if code, reason := tc.closeStatus(); code == api.TunnelCloseEnded {
			return &TunnelEndedError{Reason: reason}
		}
		if clock.Now().Sub(start) >= tunnelStable {
			failures = 0
		}
		failures++
		delay := retrier.Next(failures)
		if serveErr == nil {
			serveErr = errors.New("connection to the server lost")
		}
		notify(TunnelEvent{Kind: TunnelOffline, Attempt: failures + 1, Delay: delay, Err: serveErr})
		if !wait(delay) {
			return nil
		}
	}
}

func newTunnelSession() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// retryableConnect reports whether a failed connect may succeed later:
// network failures, server errors, rate limits other than the tunnel limit,
// and a tunnel held by another process (it may be this CLI's own lost
// connection, which the server notices within a minute).
func retryableConnect(err error) bool {
	var ne *NetworkError
	if errors.As(err, &ne) {
		return true
	}
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	switch {
	case e.Status >= 500:
		return true
	case e.Status == http.StatusConflict:
		return true
	case e.Status == http.StatusTooManyRequests:
		return e.Title != api.CodeTunnelLimit
	}
	return false
}

// tunnelConn is the WebSocket as a byte stream, remembering why it closed.
type tunnelConn struct {
	net.Conn
	ws      *websocket.Conn
	mu      sync.Mutex
	readErr error
}

func (t *tunnelConn) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	if err != nil {
		t.mu.Lock()
		if t.readErr == nil {
			t.readErr = err
		}
		t.mu.Unlock()
	}
	return n, err
}

// closeStatus is the close code and reason the server sent, or -1.
func (t *tunnelConn) closeStatus() (int, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ce websocket.CloseError
	if errors.As(t.readErr, &ce) {
		return int(ce.Code), ce.Reason
	}
	return -1, ""
}

// dialTunnel opens the tunnel's WebSocket with the API token.
func (c *Client) dialTunnel(ctx context.Context, id, session string) (*tunnelConn, error) {
	u := c.BaseURL + "/api/v1/tunnels/" + esc(id) + "/connect"
	hc := &http.Client{Timeout: tunnelHandshake}
	if c.HTTP != nil {
		hc.Transport = c.HTTP.Transport
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.Token)
	h.Set("User-Agent", c.UserAgent)
	h.Set(api.TunnelSessionHeader, session)
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: hc, HTTPHeader: h})
	if err != nil {
		if resp != nil && resp.StatusCode >= 400 {
			return nil, problemFrom(resp)
		}
		return nil, &NetworkError{Err: err}
	}
	nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	return &tunnelConn{Conn: nc, ws: ws}, nil
}

// problemFrom reads an API error from a refused WebSocket handshake.
func problemFrom(resp *http.Response) error {
	e := &Error{Status: resp.StatusCode}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if json.Unmarshal(data, e) != nil || (e.Detail == "" && e.Title == "") {
		e.Detail = strings.TrimSpace(string(bytes.ToValidUTF8(data, nil)))
		if len(e.Detail) > 300 || strings.HasPrefix(e.Detail, "<") {
			e.Detail = fmt.Sprintf("server answered %s", http.StatusText(resp.StatusCode))
		}
	}
	e.Status = resp.StatusCode
	e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	return e
}

package content

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
)

// livePath is the live reload stream (DESIGN.md "Live reload"): server-sent
// events on the artifact's own host, open only while the owner's watch holds
// a lease. Every refusal is the identical miss, so with live mode off the
// path can't be told apart from an unknown reserved path.
const livePath = reservedPrefix + "live"

const (
	// liveRetry is how long a browser waits before reconnecting a dropped
	// stream: longer than the CLI's renewal (a third of the lease), so after
	// a server restart the lease is back before pages reconnect.
	liveRetry = 25 * time.Second
	// liveWriteTimeout drops a viewer that stops reading.
	liveWriteTimeout = 10 * time.Second
	// liveBusyRetry is the Retry-After of a refused stream.
	liveBusyRetry = 60
)

// liveEvent is a "version" event: the current version's facts that the info
// panel already shows a viewer.
type liveEvent struct {
	Version   int       `json:"version"`
	Published time.Time `json:"published"`
	Size      string    `json:"size"`
	Files     int       `json:"files"`
}

// live answers GET /__redrop/live for a. The access check is the content's
// (sharing.Decide), with the visitor's grants as sent on this request; it is
// repeated, against a fresh read of the artifact, on every keep-alive and
// every change, so a revoked link, a changed password, an expired visit
// grant or a lapsed lease ends the stream. Nothing here records or consumes
// a view.
func (h *Handler) live(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	grants := sharing.Grants{Visit: h.cookieValue(r, cookieVisit), Preview: h.cookieValue(r, cookiePreview)}
	slug := a.Slug
	allowed := func(a *store.Artifact) bool {
		d := h.sharing.Decide(a, grants, time.Now())
		return (d == sharing.Serve || d == sharing.ServeOwnerPreview) && h.sharing.Live(a)
	}
	site := r.Header.Get("Sec-Fetch-Site")
	if r.Method != http.MethodGet || (site != "" && site != "same-origin") || !allowed(a) {
		h.miss(w, r)
		return
	}
	st, err := h.sharing.OpenLiveStream(a, h.clients.Network(r))
	if err != nil {
		if !errors.Is(err, sharing.ErrLiveBusy) {
			h.log.Error("open live stream", "error", err)
		}
		w.Header().Set("Retry-After", strconv.Itoa(liveBusyRetry))
		http.Error(w, "too many live streams", http.StatusServiceUnavailable)
		return
	}
	defer st.Close()

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream; charset=utf-8")
	hd.Set("X-Accel-Buffering", "no") // a buffering proxy would hold events back
	rc := http.NewResponseController(w)
	// The deadline would outlive the stream on a reused HTTP/1.1
	// connection; the server sets none of its own (no WriteTimeout).
	defer rc.SetWriteDeadline(time.Time{})
	send := func(msg string) bool {
		if err := rc.SetWriteDeadline(time.Now().Add(liveWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		if _, err := w.Write([]byte(msg)); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	sent := 0
	version := func(a *store.Artifact) string {
		if a.CurrentVersion == sent {
			return ""
		}
		sent = a.CurrentVersion
		b, _ := json.Marshal(liveEvent{Version: a.CurrentVersion, Published: a.Published.UTC(), Size: humanBytes(a.Size), Files: a.Files})
		return "event: version\ndata: " + string(b) + "\n\n"
	}
	// The first event carries the current version, so a page that missed
	// a change between its load and this request catches up.
	if !send("retry: " + strconv.FormatInt(liveRetry.Milliseconds(), 10) + "\n" + version(a)) {
		return
	}
	// recheck reads the artifact again; it reports the message to send and
	// whether the stream goes on.
	recheck := func() (string, bool) {
		a, err := h.sharing.Lookup(r.Context(), slug)
		if err != nil || !allowed(a) {
			return "event: end\ndata: {}\n\n", false
		}
		return version(a), true
	}
	tick := time.NewTicker(h.cfg.LiveKeepAlive)
	defer tick.Stop()
	for {
		var msg string
		var more bool
		select {
		case <-r.Context().Done():
			return
		case <-st.Quit():
			// Shutting down: no "end", so pages reconnect to the restarted server.
			return
		case <-st.Changed():
			msg, more = recheck()
		case <-tick.C:
			msg, more = recheck()
			if more && msg == "" {
				msg = ": keep-alive\n\n"
			}
		}
		if msg != "" && !send(msg) {
			return
		}
		if !more {
			return
		}
	}
}

package content

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/sharing"
)

// deadlineStream records the deadline net/http will inherit when it writes
// the final chunk after the handler returns. Shutdown starts after the first
// flush, so the test needs neither a socket nor a timer.
type deadlineStream struct {
	*httptest.ResponseRecorder
	deadline time.Time
	onFlush  func()
}

func (w *deadlineStream) SetWriteDeadline(at time.Time) error {
	w.deadline = at
	return nil
}

func (w *deadlineStream) Flush() {
	w.ResponseRecorder.Flush()
	if w.onFlush != nil {
		f := w.onFlush
		w.onFlush = nil
		f()
	}
}

func TestLiveEndKeepsFinalWriteBounded(t *testing.T) {
	for _, reason := range []string{"shutdown", "release", "lapsed lease"} {
		t.Run(reason, func(t *testing.T) {
			h, _, artifacts := reservedRouteFixture(t)
			a := artifacts["active"]
			actor := sharing.Actor{UserID: a.UserID}
			ctx := context.Background()
			const session = "watch-session-0001"
			if _, err := h.sharing.HoldLive(ctx, actor, a.ID, session); err != nil {
				t.Fatal(err)
			}
			w := &deadlineStream{ResponseRecorder: httptest.NewRecorder()}
			w.onFlush = func() {
				switch reason {
				case "shutdown":
					h.sharing.EndLiveStreams()
				case "release":
					if err := h.sharing.ReleaseLive(ctx, actor, a.ID, session); err != nil {
						t.Fatal(err)
					}
				case "lapsed lease":
					// Backdate the in-memory lease without waiting. A title poke
					// drives the same access/lease recheck as a keep-alive.
					// Exact expiry boundaries are tested with sharing's fake clock.
					h.cfg.LiveLease = -time.Minute
					if _, err := h.sharing.HoldLive(ctx, actor, a.ID, session); err != nil {
						t.Fatal(err)
					}
					title := "Still accessible, no longer live"
					if _, err := h.sharing.Update(ctx, actor, a.ID, sharing.Patch{Title: &title}); err != nil {
						t.Fatal(err)
					}
				}
			}
			r := httptest.NewRequest(http.MethodGet, "https://"+a.Slug+".usercontent.example"+livePath, nil)
			h.live(w, r, a)
			end := "event: end\ndata: {}\n\n"
			if reason == "shutdown" {
				end = "event: end\ndata: {\"reconnect\":true}\n\n"
			}
			if !strings.HasSuffix(w.Body.String(), end) {
				t.Errorf("wrong final event: %q", w.Body.String())
			}
			if w.Header().Get("Connection") != "close" {
				t.Error("HTTP/1.1 stream must close rather than reuse its deadline")
			}
			if w.deadline.IsZero() {
				t.Error("handler cleared the deadline before net/http's final chunk")
			}
		})
	}
}

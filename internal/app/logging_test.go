package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/mrRedSun/redrop/internal/accounts"
	"github.com/mrRedSun/redrop/internal/dashpaths"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Security review A6: one-time secrets in paths (invites, resets, setup)
// and in request bodies never reach the log, from any logger: redrop's own,
// Fuego's (which logs through the default slog logger the server installs),
// or net/http's (which the default slog logger receives too). The capture
// runs at debug level, below what the server logs in production.
func TestSecretPathsAreNeverLogged(t *testing.T) {
	var out syncBuffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	dashboard := fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>redrop</title>")}}
	h := newHarnessWith(t, log, Options{Dashboard: dashboard})
	ctx := context.Background()
	setup, err := h.app.Accounts.Bootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	setupTok := setup[strings.LastIndex(setup, "/")+1:]
	expectStatus(t, h.raw("POST", "/api/v1/setup", "application/json", strings.NewReader(`{"token":"`+setupTok+`","username":"admin","password":"long-enough-password"}`)), 200, "setup")
	admin := h.app.Accounts.Administration()
	inv, err := admin.CreateInvite(ctx, "", accounts.InviteRequest{})
	if err != nil {
		t.Fatal(err)
	}
	adminID, err := admin.UserIDByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	reset, err := admin.CreateResetLink(ctx, "", adminID, false)
	if err != nil {
		t.Fatal(err)
	}
	last := func(u string) string { return u[strings.LastIndex(u, "/")+1:] }
	invite, resetTok := last(inv.URL), last(reset.URL)
	const fake = "zzzzfaketokenzzzzzzzzzzzzzzzzzzzzzzzzzzz"
	secrets := []string{invite, resetTok, fake}

	for _, tok := range secrets {
		// Dashboard paths (served by the SPA) for every prefix.
		for _, pre := range []string{dashpaths.Invite, dashpaths.Reset, dashpaths.Setup} {
			expectStatus(t, h.raw("GET", pre+tok, "", nil), 200, "dashboard "+pre)
		}
		// An end-to-end encrypted link's intake page names a slug, the
		// link's secret; one that isn't a slug is a 404, which is logged.
		h.raw("GET", "/e/"+tok, "", nil)
		// API paths: reads, then the writes that redeem them.
		h.raw("GET", "/api/v1/invites/"+tok, "", nil)
		h.raw("GET", "/api/v1/reset/"+tok, "", nil)
	}
	expectStatus(t, h.raw("POST", "/api/v1/invites/"+invite+"/accept", "application/json", strings.NewReader(`{"username":"carol","password":"carol-password-123"}`)), 200, "accept invite")
	expectStatus(t, h.raw("POST", "/api/v1/reset/"+resetTok, "application/json", strings.NewReader(`{"password":"new-admin-password-123"}`)), 200, "reset password")
	h.raw("POST", "/api/v1/invites/"+fake+"/accept", "application/json", strings.NewReader(`{"username":"dave","password":"dave-password-123"}`))
	h.raw("POST", "/api/v1/reset/"+fake, "application/json", strings.NewReader(`{"password":"whatever-password"}`))

	logged := out.String()
	for _, secret := range append(secrets, setupTok, "long-enough-password", "carol-password-123", "new-admin-password-123") {
		if strings.Contains(logged, secret) {
			t.Errorf("a secret reached the log:\n%s", logged)
			break
		}
	}
	for _, pre := range dashpaths.SecretPrefixes {
		if pre == dashpaths.Setup || pre == dashpaths.Invite || pre == dashpaths.Reset {
			continue // dashboard pages are logged only on failure
		}
		if !strings.Contains(logged, "path="+pre+"…") {
			t.Errorf("the capture missed redrop's redacted request log for %s", pre)
		}
	}
}

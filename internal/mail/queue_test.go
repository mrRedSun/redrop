package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	stdmail "net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/store"
)

const testInvitePrefix = "https://app.example.com/invite/"

type mailSignalLog struct {
	slog.Handler
	hit   chan struct{}
	count atomic.Int32
}

func (h *mailSignalLog) Handle(ctx context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "failed") {
		h.count.Add(1)
		select {
		case h.hit <- struct{}{}:
		default:
		}
	}
	return nil
}

func mailText(t *testing.T, raw []byte) string {
	t.Helper()
	m, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := multipart.NewReader(m.Body, params["boundary"]).NextPart()
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, p))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// inviteToken extracts the one-time token from a sent invite email.
func inviteToken(t *testing.T, raw []byte) string {
	t.Helper()
	text := mailText(t, raw)
	i := strings.Index(text, testInvitePrefix)
	if i < 0 {
		t.Fatalf("no invite link in email:\n%s", text)
	}
	token, _, _ := strings.Cut(text[i+len(testInvitePrefix):], "\n")
	return strings.TrimSpace(token)
}

func messageID(t *testing.T, raw []byte) string {
	t.Helper()
	m, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return m.Header.Get("Message-ID")
}

// redeemable reports whether token is the invite's current, working link.
func redeemable(t *testing.T, db *store.Store, token string) bool {
	t.Helper()
	_, err := db.OneTimeByHash(context.Background(), store.PurposeInvite, ids.Hash(token))
	return err == nil
}

type sendFunc func(context.Context, Message) error

func (f sendFunc) Send(c context.Context, m Message) error { return f(c, m) }

// recorder keeps every message handed to the relay.
type recorder struct {
	mu   sync.Mutex
	sent [][]byte
	err  func(n int) error // the result of the n-th send, from 1; nil = accept
}

func (r *recorder) Send(_ context.Context, m Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, append([]byte(nil), m.Raw...))
	if r.err != nil {
		return r.err(len(r.sent))
	}
	return nil
}

func (r *recorder) messages() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.sent...)
}

func testQueue(t *testing.T, m Mailer) (*Queue, *store.Store) {
	return testQueueWith(t, m, DefaultQueueConfig())
}

func testQueueWith(t *testing.T, m Mailer, cfg QueueConfig) (*Queue, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return newTestQueue(t, db, m, cfg), db
}

func newTestQueue(t *testing.T, db *store.Store, m Mailer, cfg QueueConfig) *Queue {
	t.Helper()
	q, err := NewQueue(db, m, "Redrop <share@example.com>", slog.New(slog.DiscardHandler), cfg)
	if err != nil {
		t.Fatal(err)
	}
	q.SetInviteLink(func(secret string) string { return testInvitePrefix + secret })
	t.Cleanup(q.Close)
	return q
}

// queuedInvite stores an emailed invite as the accounts service does: with
// the hash of a secret nobody keeps, and a queue row without a body.
func queuedInvite(t *testing.T, db *store.Store, actor string, at time.Time, ttl time.Duration) *store.OneTime {
	t.Helper()
	o := &store.OneTime{Purpose: store.PurposeInvite, Email: "recipient@example.com", Role: "user", Note: "Hello <friend>", ExpiresAt: at.Add(ttl)}
	row := &store.OutgoingMail{To: o.Email, Inviter: "an admin"}
	if err := db.CreateMailedInvite(context.Background(), o, ids.Hash(ids.Secret()), actor, row, at); err != nil {
		t.Fatal(err)
	}
	return o
}

func claim(t *testing.T, q *Queue, at time.Time) (*store.OutgoingMail, string) {
	t.Helper()
	m, secret, err := q.claim(context.Background(), at)
	if err != nil || m == nil {
		t.Fatalf("claim: %v", err)
	}
	return m, secret
}

func mailRow(t *testing.T, db *store.Store, id string) *store.OutgoingMail {
	t.Helper()
	m, err := db.MailByInvite(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// H-1: no invite token is stored in any form. The row has no body, and a
// copy of the whole database holds neither the sent token nor the link.
func TestInviteTokenIsNeverStored(t *testing.T) {
	r := &recorder{}
	q, db := testQueue(t, r)
	ctx := context.Background()
	at := time.Now()
	o := queuedInvite(t, db, "offline", at, time.Hour)
	if row := mailRow(t, db, o.ID); len(row.Body) != 0 {
		t.Fatal("queued invite row has a body")
	}
	m, secret := claim(t, q, at.Add(time.Second))
	if err := q.deliver(ctx, m, secret); err != nil {
		t.Fatal(err)
	}
	token := inviteToken(t, r.messages()[0])
	if token != secret || !redeemable(t, db, token) {
		t.Fatal("the emailed link is not the invite's working link")
	}
	if row := mailRow(t, db, o.ID); len(row.Body) != 0 || row.Status != store.MailSent {
		t.Fatalf("sent invite row: status %s, %d body bytes", row.Status, len(row.Body))
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := db.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(token)) {
		t.Fatal("a database copy holds the invite token")
	}
}

// H-1, m-C: Send again rotates the token, so the previous email's link stops
// working, and the new email is a new message (Message-ID, Date).
func TestResendRotatesTokenAndMessage(t *testing.T) {
	r := &recorder{}
	q, db := testQueue(t, r)
	ctx := context.Background()
	at := time.Now()
	o := queuedInvite(t, db, "offline", at, time.Hour)
	m, secret := claim(t, q, at.Add(time.Second))
	if err := q.deliver(ctx, m, secret); err != nil {
		t.Fatal(err)
	}
	if err := db.ResendInviteMail(ctx, o.ID, "offline", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	m, secret = claim(t, q, at.Add(2*time.Second))
	if err := q.deliver(ctx, m, secret); err != nil {
		t.Fatal(err)
	}
	sent := r.messages()
	if len(sent) != 2 {
		t.Fatalf("%d emails", len(sent))
	}
	first, second := inviteToken(t, sent[0]), inviteToken(t, sent[1])
	if first == second || redeemable(t, db, first) || !redeemable(t, db, second) {
		t.Fatal("Send again did not replace the link")
	}
	if messageID(t, sent[0]) == messageID(t, sent[1]) {
		t.Fatal("Send again reused the Message-ID")
	}
	if err := db.DeleteOneTime(ctx, store.PurposeInvite, o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MailByInvite(ctx, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("revoking invite retained mail")
	}
}

// H-1: a crash after the token was rotated but before SMTP loses nothing:
// recovery claims the row again, mints another token, and that link works.
func TestCrashAfterRotationSendsWorkingLink(t *testing.T) {
	q, db := testQueue(t, Discard{})
	at := time.Now()
	o := queuedInvite(t, db, "offline", at, time.Hour)
	_, lost := claim(t, q, at.Add(time.Second)) // the process dies holding it
	if !redeemable(t, db, lost) {
		t.Fatal("claim did not rotate the token")
	}
	r := &recorder{}
	next := newTestQueue(t, db, r, DefaultQueueConfig())
	if err := next.Start(); err != nil { // recovery returns the row to queued
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for mailRow(t, db, o.ID).Status != store.MailSent {
		if time.Now().After(deadline) {
			t.Fatal("recovered invite was not sent")
		}
		time.Sleep(time.Millisecond)
	}
	token := inviteToken(t, r.messages()[0])
	if token == lost || redeemable(t, db, lost) || !redeemable(t, db, token) {
		t.Fatal("recovery did not send a fresh, working link")
	}
	if got := mailRow(t, db, o.ID).Attempts; got != 2 {
		t.Fatalf("attempts %d, want 2", got)
	}
}

// H-1: every attempt, automatic retry or after a restart, mints its own
// token, and only the latest one works.
func TestRetriesAndRestartsRotateToken(t *testing.T) {
	r := &recorder{err: func(n int) error {
		if n < 3 {
			return &SMTPError{Stage: StageRecipient, Code: 451}
		}
		return nil
	}}
	cfg := QueueConfig{MaxAttempts: 4, Backoff: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
	q, db := testQueueWith(t, r, cfg)
	ctx := context.Background()
	at := time.Now()
	o := queuedInvite(t, db, "offline", at, time.Hour)
	var minted []string
	// Attempt 1 fails and is retried.
	m, secret := claim(t, q, at.Add(time.Second))
	minted = append(minted, secret)
	if err := q.deliver(ctx, m, secret); err != nil {
		t.Fatal(err)
	}
	// Attempt 2 is claimed, then the process restarts before SMTP.
	_, secret = claim(t, q, at.Add(time.Minute))
	minted = append(minted, secret)
	if err := db.RecoverMail(ctx); err != nil {
		t.Fatal(err)
	}
	// Attempt 3 fails at the relay; attempt 4 is accepted.
	for range 2 {
		m, secret = claim(t, q, at.Add(time.Minute))
		minted = append(minted, secret)
		if err := q.deliver(ctx, m, secret); err != nil {
			t.Fatal(err)
		}
	}
	if row := mailRow(t, db, o.ID); row.Status != store.MailSent || row.Attempts != 4 {
		t.Fatalf("final state %s after %d attempts", row.Status, row.Attempts)
	}
	seen := map[string]bool{}
	for i, s := range minted {
		if seen[s] {
			t.Fatal("a token was minted twice")
		}
		seen[s] = true
		if want := i == len(minted)-1; redeemable(t, db, s) != want {
			t.Fatalf("token %d redeemable=%v, want %v", i+1, !want, want)
		}
	}
	sent := r.messages()
	if inviteToken(t, sent[len(sent)-1]) != minted[len(minted)-1] {
		t.Fatal("the accepted email does not carry the working token")
	}
}

// m-E: a redemption that has already looked the old token up, and then spends
// its password-hashing time, must not consume the invite when a Send again
// claim rotates the hash in between. Redeem matches the hash, not just the ID.
func TestRedeemAfterRotationIsRefused(t *testing.T) {
	r := &recorder{}
	q, db := testQueue(t, r)
	ctx := context.Background()
	at := time.Now()
	o := queuedInvite(t, db, "offline", at, time.Hour)
	m, s1 := claim(t, q, at.Add(time.Second))
	if err := q.deliver(ctx, m, s1); err != nil {
		t.Fatal(err)
	}
	// The holder of the first email starts redeeming: the lookup succeeds.
	looked, err := db.OneTimeByHash(ctx, store.PurposeInvite, ids.Hash(s1))
	if err != nil {
		t.Fatal(err)
	}
	ph, err := security.HashPassword("probe-password-long")
	if err != nil {
		t.Fatal(err)
	}
	// Meanwhile the admin's Send again is claimed: the hash rotates, s1 dies.
	if err := db.ResendInviteMail(ctx, o.ID, "offline", at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	m, s2, err := q.claim(ctx, at.Add(3*time.Second))
	if err != nil || m == nil {
		t.Fatalf("resend claim: %v", err)
	}
	if err := q.deliver(ctx, m, s2); err != nil {
		t.Fatal(err)
	}
	if redeemable(t, db, s1) || !redeemable(t, db, s2) {
		t.Fatal("rotation did not replace the link")
	}
	// The stale lookup must be refused instead of consuming the invite.
	if _, err := db.RedeemAccountLink(ctx, looked, ids.Hash(s1), "oldlinkholder", "oldlinkholder", ph, 1<<30, "", "", ""); !errors.Is(err, store.ErrLinkUsed) {
		t.Fatalf("redeem with the looked-up old link after rotation: %v", err)
	}
	if !redeemable(t, db, s2) {
		t.Fatal("the refused redemption consumed the invite")
	}
}

// m-F: a crash during the last allowed attempt, before the outcome is saved,
// must not rotate the hash when the row is recovered: the email that did go
// out keeps its working link, and the row says to use Send again.
func TestCrashOnLastAttemptKeepsDeliveredLink(t *testing.T) {
	r := &recorder{err: func(n int) error { return &SMTPError{Stage: StageRecipient, Code: 451} }}
	cfg := QueueConfig{MaxAttempts: 4, Backoff: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
	q, db := testQueueWith(t, r, cfg)
	ctx := context.Background()
	at := time.Now()
	o := queuedInvite(t, db, "offline", at, time.Hour)
	for range 3 {
		m, s := claim(t, q, at.Add(time.Minute))
		if err := q.deliver(ctx, m, s); err != nil {
			t.Fatal(err)
		}
	}
	// Attempt 4: the relay delivers, then the process dies before the
	// outcome is written and recovery re-queues the row.
	r.err = nil
	_, s4 := claim(t, q, at.Add(time.Minute))
	if !redeemable(t, db, s4) {
		t.Fatal("the delivered email's link is not the working link")
	}
	before := len(r.messages())
	if err := db.RecoverMail(ctx); err != nil {
		t.Fatal(err)
	}
	// The next claim fails the row without rotating the hash.
	if _, _, err := q.claim(ctx, at.Add(2*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the recovered row was claimed: %v", err)
	}
	row := mailRow(t, db, o.ID)
	if row.Status != store.MailFailed || !strings.Contains(row.Error, "Send again") || row.Attempts != 4 {
		t.Fatalf("recovered-at-limit row: %s %q attempts=%d", row.Status, row.Error, row.Attempts)
	}
	if !redeemable(t, db, s4) {
		t.Fatal("recovery killed the delivered email's link")
	}
	if n := len(r.messages()) - before; n != 0 {
		t.Fatalf("%d emails sent after the crash", n)
	}
}

func TestQueueRetryScheduleAndPermanentRecipient(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient", true: "permanent"}[permanent], func(t *testing.T) {
			calls := 0
			cfg := QueueConfig{MaxAttempts: 4, Backoff: []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}}
			q, db := testQueueWith(t, sendFunc(func(_ context.Context, _ Message) error {
				calls++
				if permanent {
					return &PermanentError{Stage: StageRecipient, Code: 550, Recipient: true}
				}
				return &SMTPError{Stage: StageRecipient, Code: 451}
			}), cfg)
			ctx := context.Background()
			at := time.Now()
			o := queuedInvite(t, db, "offline", at, time.Hour)
			for attempt := 1; attempt <= 4; attempt++ {
				m, secret := claim(t, q, at.Add(time.Hour/2))
				if err := q.deliver(ctx, m, secret); err != nil {
					t.Fatal(err)
				}
				row := mailRow(t, db, o.ID)
				if permanent || attempt == 4 {
					if row.Status != store.MailFailed {
						t.Fatal("not terminal")
					}
					break
				}
				if row.Status != store.MailQueued || row.Attempts != attempt {
					t.Fatalf("retry state: %+v", row)
				}
				if _, _, err := q.claim(ctx, at.Add(-time.Second)); !errors.Is(err, store.ErrNotFound) {
					t.Fatal("retry happened before due")
				}
			}
			if permanent && calls != 1 || !permanent && calls != 4 {
				t.Fatalf("attempts %d", calls)
			}
			err := db.ResendInviteMail(ctx, o.ID, "offline", at.Add(time.Minute))
			if permanent && !errors.Is(err, store.ErrMailBounced) {
				t.Fatalf("bounce suppression: %v", err)
			}
			if !permanent && err != nil {
				t.Fatal(err)
			}
			if permanent {
				if removed, err := db.UnbounceMail(ctx, "recipient@example.com"); err != nil || !removed {
					t.Fatalf("unbounce: %v %v", removed, err)
				}
				if err := db.ResendInviteMail(ctx, o.ID, "offline", at.Add(time.Minute)); err != nil {
					t.Fatalf("resend after unbounce: %v", err)
				}
			}
		})
	}
}

// m-D: the production schedule spans minutes, and an invite is not retried
// past its expiry.
func TestInviteBackoffIsMinutesAndBoundedByExpiry(t *testing.T) {
	cfg := DefaultQueueConfig()
	if cfg.MaxAttempts != 4 || cfg.Backoff[0] < time.Minute || cfg.Backoff[2] < 30*time.Minute {
		t.Fatalf("backoff %v is too short for a relay restart", cfg.Backoff)
	}
	q, db := testQueue(t, sendFunc(func(context.Context, Message) error { return &SMTPError{Stage: StageRecipient, Code: 451} }))
	ctx := context.Background()
	at := time.Now()
	long := queuedInvite(t, db, "offline", at, 24*time.Hour)
	m, secret := claim(t, q, at.Add(time.Second))
	if err := q.deliver(ctx, m, secret); err != nil {
		t.Fatal(err)
	}
	if row := mailRow(t, db, long.ID); row.Status != store.MailQueued {
		t.Fatalf("a 451 failed a day-long invite: %s", row.Status)
	}
	if _, _, err := q.claim(ctx, at.Add(30*time.Second)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("retried within seconds")
	}
	short := queuedInvite(t, db, "offline", at, 30*time.Second)
	m, secret = claim(t, q, at.Add(time.Second))
	if m.InviteID != short.ID {
		t.Fatal("claimed the wrong row")
	}
	if err := q.deliver(ctx, m, secret); err != nil {
		t.Fatal(err)
	}
	if row := mailRow(t, db, short.ID); row.Status != store.MailFailed || !strings.Contains(row.Error, "expires before the next retry") {
		t.Fatalf("retry scheduled past expiry: %s %q", row.Status, row.Error)
	}
}

func TestAuthenticationRefusalDoesNotBounceRecipient(t *testing.T) {
	q, db := testQueue(t, sendFunc(func(context.Context, Message) error { return &PermanentError{Stage: StageAuth, Code: 535} }))
	ctx := context.Background()
	at := time.Now()
	o := queuedInvite(t, db, "offline", at, time.Hour)
	m, secret := claim(t, q, at.Add(time.Second))
	if err := q.deliver(ctx, m, secret); err != nil {
		t.Fatal(err)
	}
	if err := db.ResendInviteMail(ctx, o.ID, "offline", at.Add(time.Minute)); err != nil {
		t.Fatalf("auth failure poisoned recipient: %v", err)
	}
}

func TestQueueErrorsNeverLogLink(t *testing.T) {
	var minted string
	q, db := testQueue(t, sendFunc(func(context.Context, Message) error {
		return errors.New("remote echo " + testInvitePrefix + minted)
	}))
	var logs bytes.Buffer
	q.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	at := time.Now()
	o := queuedInvite(t, db, "offline", at, time.Hour)
	m, secret := claim(t, q, at.Add(time.Second))
	minted = secret
	if err := q.deliver(context.Background(), m, secret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(mailRow(t, db, o.ID).Error, secret) {
		t.Fatal("remote text leaked")
	}
}

func TestOutcomeWriteFailureDoesNotResendSMTP(t *testing.T) {
	var calls atomic.Int32
	q, db := testQueue(t, sendFunc(func(context.Context, Message) error { calls.Add(1); return nil }))
	at := time.Now()
	o := queuedInvite(t, db, "offline", at, time.Hour)
	if _, err := db.DB.Exec(`CREATE TRIGGER reject_outcome BEFORE UPDATE ON mail_out WHEN NEW.status='sent' BEGIN SELECT RAISE(ABORT,'test write failure'); END;`); err != nil {
		t.Fatal(err)
	}
	// DiscardHandler's Enabled is false; use an enabled handler.
	h := &mailSignalLog{Handler: slog.NewTextHandler(io.Discard, nil), hit: make(chan struct{}, 1)}
	q.log = slog.New(h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	m, secret := claim(t, q, at.Add(time.Second))
	go func() { done <- q.deliver(ctx, m, secret) }()
	select {
	case <-h.hit:
	case <-time.After(5 * time.Second):
		t.Fatal("did not attempt outcome write")
	}
	if _, err := db.DB.Exec(`DROP TRIGGER reject_outcome`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("outcome was not retried")
	}
	if calls.Load() != 1 || mailRow(t, db, o.ID).Status != store.MailSent {
		t.Fatal("database write retry resent message or left it stuck")
	}
}

func TestAlertWriteFailureUsesErrorBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q, db := testQueue(t, Discard{})
		at := time.Now()
		q.SetAlerts([]string{"ops@example.com"}, time.Second, "https://app.example.com/admin/reports")
		if err := db.InitMailAlerts(t.Context(), q.addresses, q.interval, at); err != nil {
			t.Fatal(err)
		}
		addAlertReports(t, db, 1, at)
		if _, err := db.DB.Exec(`CREATE TRIGGER reject_alert BEFORE INSERT ON mail_out BEGIN SELECT RAISE(ABORT,'test write failure'); END;`); err != nil {
			t.Fatal(err)
		}
		h := &mailSignalLog{Handler: slog.NewTextHandler(io.Discard, nil), hit: make(chan struct{}, 1)}
		q.log = slog.New(h)
		if err := q.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		q.Close()
		if n := h.count.Load(); n < 1 || n > 6 {
			t.Fatalf("%d errors in five fake seconds; past deadline bypassed backoff", n)
		}
	})
}

func TestWorkerShutdownAndRestart(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	q, db := testQueue(t, sendFunc(func(ctx context.Context, _ Message) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	}))
	o := queuedInvite(t, db, "offline", time.Now(), time.Hour)
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	q.Close()
	if got := mailRow(t, db, o.ID); got.Status != store.MailQueued || got.Attempts != 1 {
		t.Fatalf("shutdown: %+v", got)
	}
	next := newTestQueue(t, db, Discard{}, DefaultQueueConfig())
	if err := next.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if m := mailRow(t, db, o.ID); m.Status == store.MailSent {
			if m.Attempts != 2 {
				t.Fatal("lost attempt count")
			}
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("restart did not send")
		case <-tick.C:
		}
	}
}

func TestStartNeedsInviteLink(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q, err := NewQueue(db, Discard{}, "share@example.com", slog.New(slog.DiscardHandler), DefaultQueueConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Start(); err == nil {
		q.Close()
		t.Fatal("started without a way to build invite links")
	}
}

func TestQueueAdmissionBoundsAreAtomic(t *testing.T) {
	_, db := testQueue(t, Discard{})
	at := time.Now()
	ctx := context.Background()
	for range 10 {
		queuedInvite(t, db, "admin-a", at, time.Hour)
	}
	o := &store.OneTime{Role: "user", ExpiresAt: at.Add(time.Hour)}
	row := &store.OutgoingMail{To: "next@example.com"}
	if err := db.CreateMailedInvite(ctx, o, "unused-hash", "admin-a", row, at); !errors.Is(err, store.ErrMailRate) {
		t.Fatalf("per-admin cap: %v", err)
	}
	for a := 0; a < 5; a++ {
		for range 10 {
			queuedInvite(t, db, string(rune('b'+a)), at, time.Hour)
		}
	}
	if err := db.CreateMailedInvite(ctx, o, "unused-hash", "other", row, at); !errors.Is(err, store.ErrMailRate) {
		t.Fatalf("global cap: %v", err)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT count(*) FROM onetime`).Scan(&n); err != nil || n != 60 {
		t.Fatalf("orphan invites: %d %v", n, err)
	}
	// A new hour restores the rate budget, but not the queue's disk bound.
	for hour := 1; hour <= 2; hour++ {
		for i := 0; i < min(60, store.MaxPendingMail-60-(hour-1)*60); i++ {
			queuedInvite(t, db, string(rune('a'+i/10)), at.Add(time.Duration(hour)*time.Hour), 4*time.Hour)
		}
	}
	if err := db.CreateMailedInvite(ctx, o, "unused-hash", "other", row, at.Add(3*time.Hour)); !errors.Is(err, store.ErrMailFull) {
		t.Fatalf("queue cap: %v", err)
	}
}

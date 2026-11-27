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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/store"
)

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

func TestOutcomeWriteFailureDoesNotResendSMTP(t *testing.T) {
	var calls atomic.Int32
	q, db := testQueue(t, sendFunc(func(context.Context, Message) error { calls.Add(1); return nil }))
	at := time.Now()
	o, _ := queuedInvite(t, q, db, "offline", at)
	if _, err := db.DB.Exec(`CREATE TRIGGER reject_outcome BEFORE UPDATE ON mail_out WHEN NEW.status='sent' BEGIN SELECT RAISE(ABORT,'test write failure'); END;`); err != nil {
		t.Fatal(err)
	}
	h := &mailSignalLog{Handler: slog.DiscardHandler, hit: make(chan struct{}, 1)}
	// DiscardHandler's Enabled is false; explicitly use an enabled handler.
	h.Handler = slog.NewTextHandler(io.Discard, nil)
	q.log = slog.New(h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	m := claim(t, db, at.Add(time.Second))
	go func() { done <- q.deliver(ctx, m) }()
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

type sendFunc func(context.Context, Message) error

func (f sendFunc) Send(c context.Context, m Message) error { return f(c, m) }

var queueSecret = []byte("test-mail-queue-secret-at-least-32-bytes")

func testQueue(t *testing.T, m Mailer) (*Queue, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	q, err := NewQueue(db, m, "Redrop <share@example.com>", queueSecret, slog.New(slog.DiscardHandler), DefaultQueueConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	return q, db
}
func queuedInvite(t *testing.T, q *Queue, db *store.Store, actor string, at time.Time) (*store.OneTime, string) {
	t.Helper()
	secret := ids.Secret()
	link := "https://app.example.com/invite/" + secret
	o := &store.OneTime{Purpose: store.PurposeInvite, Email: "recipient@example.com", Role: "user", Note: "Hello <friend>", ExpiresAt: at.Add(time.Hour)}
	m, err := (InviteEmail{Link: link, Note: o.Note, Inviter: "an admin", Expires: o.ExpiresAt}).Render()
	if err != nil {
		t.Fatal(err)
	}
	m.To = o.Email
	row, err := q.Prepare(m, "invite", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateMailedInvite(context.Background(), o, ids.Hash(secret), actor, row, at); err != nil {
		t.Fatal(err)
	}
	return o, link
}
func claim(t *testing.T, db *store.Store, at time.Time) *store.OutgoingMail {
	t.Helper()
	m, err := db.ClaimMail(context.Background(), at, time.Minute)
	if err != nil || m == nil {
		t.Fatalf("claim: %v", err)
	}
	return m
}
func mailRow(t *testing.T, db *store.Store, id string) *store.OutgoingMail {
	t.Helper()
	m, err := db.MailByInvite(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestQueuePersistsEncryptedInviteAndResend(t *testing.T) {
	var sent []Message
	q, db := testQueue(t, sendFunc(func(_ context.Context, m Message) error { sent = append(sent, m); return nil }))
	ctx := context.Background()
	at := time.Now()
	o, link := queuedInvite(t, q, db, "offline", at)
	row := mailRow(t, db, o.ID)
	if bytes.Contains(row.Body, []byte(link)) || bytes.Contains(row.Body, []byte("recipient@example.com")) {
		t.Fatal("plaintext body persisted")
	}
	plain, err := q.decrypt(row)
	if err != nil {
		t.Fatal(err)
	}
	swapped := *row
	swapped.To = "another@example.com"
	if _, err := q.decrypt(&swapped); err == nil {
		t.Fatal("swapped row decrypted")
	}
	if err := q.deliver(ctx, claim(t, db, at.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if got := mailRow(t, db, o.ID); got.Status != store.MailSent || got.Attempts != 1 {
		t.Fatalf("outcome: %+v", got)
	}
	if err := db.ResendInviteMail(ctx, o.ID, "offline", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := q.deliver(ctx, claim(t, db, at.Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || !bytes.Equal(sent[0].Raw, plain) || !bytes.Equal(sent[0].Raw, sent[1].Raw) {
		t.Fatal("resend changed original message")
	}
	if err := db.DeleteOneTime(ctx, store.PurposeInvite, o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MailByInvite(ctx, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("revoking invite retained mail")
	}
}

func TestQueueRetryScheduleAndPermanentRecipient(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient", true: "permanent"}[permanent], func(t *testing.T) {
			calls := 0
			q, db := testQueue(t, sendFunc(func(_ context.Context, _ Message) error {
				calls++
				if permanent {
					return &PermanentError{Stage: StageRecipient, Code: 550, Recipient: true}
				}
				return &SMTPError{Stage: StageRecipient, Code: 451}
			}))
			ctx := context.Background()
			at := time.Now()
			o, _ := queuedInvite(t, q, db, "offline", at)
			for attempt := 1; attempt <= 4; attempt++ {
				m := claim(t, db, at.Add(time.Hour/2))
				if err := q.deliver(ctx, m); err != nil {
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
				if _, err := db.ClaimMail(ctx, at.Add(-time.Second), time.Minute); !errors.Is(err, store.ErrNotFound) {
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
		})
	}
}

func TestAuthenticationRefusalDoesNotBounceRecipient(t *testing.T) {
	q, db := testQueue(t, sendFunc(func(context.Context, Message) error { return &PermanentError{Stage: StageAuth, Code: 535} }))
	ctx := context.Background()
	at := time.Now()
	o, _ := queuedInvite(t, q, db, "offline", at)
	if err := q.deliver(ctx, claim(t, db, at.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := db.ResendInviteMail(ctx, o.ID, "offline", at.Add(time.Minute)); err != nil {
		t.Fatalf("auth failure poisoned recipient: %v", err)
	}
}

func TestQueueErrorsNeverLogLink(t *testing.T) {
	const secret = "ONE-TIME-LINK-SECRET"
	q, db := testQueue(t, sendFunc(func(context.Context, Message) error {
		return errors.New("remote echo https://app.example.com/invite/" + secret)
	}))
	var logs bytes.Buffer
	q.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	at := time.Now()
	o, _ := queuedInvite(t, q, db, "offline", at)
	if err := q.deliver(context.Background(), claim(t, db, at.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(mailRow(t, db, o.ID).Error, secret) {
		t.Fatal("remote text leaked")
	}
}

func TestWorkerShutdownAndRestart(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	q, db := testQueue(t, sendFunc(func(ctx context.Context, _ Message) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	}))
	o, _ := queuedInvite(t, q, db, "offline", time.Now())
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
	next, err := NewQueue(db, sendFunc(func(context.Context, Message) error { return nil }), q.from, queueSecret, q.log, DefaultQueueConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err = next.Start(); err != nil {
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

func TestQueueAdmissionBoundsAreAtomic(t *testing.T) {
	q, db := testQueue(t, Discard{})
	at := time.Now()
	ctx := context.Background()
	for range 10 {
		queuedInvite(t, q, db, "admin-a", at)
	}
	o := &store.OneTime{Role: "user", ExpiresAt: at.Add(time.Hour)}
	row, err := q.Prepare(Message{To: "next@example.com", Subject: "test", Text: "test"}, "invite", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CreateMailedInvite(ctx, o, "unused-hash", "admin-a", row, at); !errors.Is(err, store.ErrMailRate) {
		t.Fatalf("per-admin cap: %v", err)
	}
	for a := 0; a < 5; a++ {
		for range 10 {
			queuedInvite(t, q, db, string(rune('b'+a)), at)
		}
	}
	if err = db.CreateMailedInvite(ctx, o, "unused-hash", "other", row, at); !errors.Is(err, store.ErrMailRate) {
		t.Fatalf("global cap: %v", err)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT count(*) FROM onetime`).Scan(&n); err != nil || n != 60 {
		t.Fatalf("orphan invites: %d %v", n, err)
	}
	// A new hour restores the rate budget, but not the queue's disk bound.
	for hour := 1; hour <= 2; hour++ {
		for i := 0; i < min(60, store.MaxPendingMail-60-(hour-1)*60); i++ {
			queuedInvite(t, q, db, string(rune('a'+i/10)), at.Add(time.Duration(hour)*time.Hour))
		}
	}
	if err = db.CreateMailedInvite(ctx, o, "unused-hash", "other", row, at.Add(3*time.Hour)); !errors.Is(err, store.ErrMailFull) {
		t.Fatalf("queue cap: %v", err)
	}
}

package mail

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	idgen "github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/store"
)

func addAlertReports(t *testing.T, db *store.Store, n int, at time.Time) []string {
	t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		id := idgen.ID()
		_, err := db.DB.Exec(`INSERT INTO reports(id,source,artifact_id,owner_id,category,details,contact,url,created_at) VALUES(?,'visitor','artifact','owner','spam','UNTRUSTED ARTIFACT CONTENT','private@example.com','https://secret.example.com/',?)`, id, at.Unix())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func claimAlert(q *Queue, at time.Time) (*store.OutgoingMail, error) {
	m, _, err := q.claim(context.Background(), at)
	return m, err
}

func TestAlertBatchingCursorAndRestart(t *testing.T) {
	q, db := testQueue(t, Discard{})
	ctx := context.Background()
	at := time.Now()
	interval := time.Minute
	q.SetAlerts([]string{"one@example.com", "two@example.com"}, interval, "https://app.example.com/admin/reports")
	addAlertReports(t, db, 1, at.Add(-time.Hour)) // historical reports are not an initial flood
	if err := db.InitMailAlerts(ctx, q.addresses, interval, at); err != nil {
		t.Fatal(err)
	}
	first := addAlertReports(t, db, 2, at)
	if err := q.queueAlerts(ctx, at.Add(interval-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := claimAlert(q, at.Add(interval)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("digest sent early")
	}
	if err := q.queueAlerts(ctx, at.Add(interval)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		m, err := claimAlert(q, at.Add(interval))
		if err != nil {
			t.Fatal(err)
		}
		text := mailText(t, m.Body)
		for _, id := range first {
			if !strings.Contains(text, id) {
				t.Fatal("missing new report")
			}
		}
		for _, private := range []string{"UNTRUSTED", "private@example.com", "secret.example.com"} {
			if strings.Contains(text, private) {
				t.Fatalf("alert leaked %s", private)
			}
		}
		if !strings.Contains(text, q.queueURL) {
			t.Fatal("no moderation link")
		}
		if err = q.db.CompleteMail(ctx, m, store.MailSent, "", time.Time{}, false, at.Add(interval)); err != nil {
			t.Fatal(err)
		}
	}
	// New reports in exactly the same created_at second must not be lost.
	next := addAlertReports(t, db, 1, at)
	if err := q.queueAlerts(ctx, at.Add(interval+time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := claimAlert(q, at.Add(interval+time.Second)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("more than one digest per interval")
	}
	// A restart preserves both the cursor and interval deadline.
	if err := db.InitMailAlerts(ctx, q.addresses, interval, at.Add(interval+time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := q.queueAlerts(ctx, at.Add(2*interval)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		m, err := claimAlert(q, at.Add(2*interval))
		if err != nil {
			t.Fatal(err)
		}
		text := mailText(t, m.Body)
		if !strings.Contains(text, next[0]) {
			t.Fatal("same-second report lost")
		}
		if strings.Count(text, "(report ") != 1 {
			t.Fatalf("duplicate reports: %s", text)
		}
	}
}

// m-A: under a steady report stream, a digest goes out every interval. The
// previous digest's claim is stamped a moment after its enqueue; that must
// not push the next digest a whole interval further.
func TestAlertStreamGetsOneDigestPerInterval(t *testing.T) {
	q, db := testQueue(t, Discard{})
	ctx := context.Background()
	interval := 20 * time.Second
	start := time.Now()
	q.SetAlerts([]string{"ops@example.com"}, interval, "https://app.example.com/admin/reports")
	if err := db.InitMailAlerts(ctx, q.addresses, interval, start.Add(-interval)); err != nil {
		t.Fatal(err)
	}
	claimed := start
	for window := range 3 {
		addAlertReports(t, db, 1, claimed)
		due := start.Add(time.Duration(window) * interval)
		if window > 0 {
			// At the old deadline the digest waits for one interval after
			// the last claim at most, not a whole interval from now: either
			// it was deferred to then, or the enqueue that just happened
			// already scheduled it no later. Assert the bound, not the exact
			// path, because the enqueue runs a moment before the claim.
			var next int64
			if err := q.queueAlerts(ctx, due); err != nil {
				t.Fatal(err)
			}
			if err := db.DB.QueryRow(`SELECT next_at FROM mail_alerts`).Scan(&next); err != nil {
				t.Fatal(err)
			}
			if want := claimed.Add(interval); time.Unix(0, next).After(want) {
				t.Fatalf("window %d: next digest at %v, a window was skipped (want no later than %v)", window, time.Unix(0, next).Sub(start), want.Sub(start))
			}
			due = claimed.Add(interval)
		}
		if err := q.queueAlerts(ctx, due); err != nil {
			t.Fatal(err)
		}
		claimed = due.Add(3 * time.Millisecond) // the worker's claim lags its enqueue
		m, err := claimAlert(q, claimed)
		if err != nil {
			t.Fatalf("window %d: no digest: %v", window, err)
		}
		if err := db.CompleteMail(ctx, m, store.MailSent, "", time.Time{}, false, claimed); err != nil {
			t.Fatal(err)
		}
		if gap := claimed.Sub(start); gap > time.Duration(window)*interval+time.Duration(window+1)*10*time.Millisecond {
			t.Fatalf("window %d: digest %v after start, a window was skipped", window, gap)
		}
	}
}

// m-B: a suppressed alert recipient does not hold up the others, and the
// hold is logged once (by position, never by address) until it clears.
func TestHeldDigestIsLoggedOnceAndSparesOthers(t *testing.T) {
	q, db := testQueue(t, Discard{})
	var logs bytes.Buffer
	q.log = slog.New(slog.NewTextHandler(&logs, nil))
	ctx := context.Background()
	interval := time.Minute
	at := time.Now()
	q.SetAlerts([]string{"refused@example.com", "ops@example.com"}, interval, "https://app.example.com/admin/reports")
	if err := db.InitMailAlerts(ctx, q.addresses, interval, at.Add(-interval)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO mail_bounced(address,at) VALUES('refused@example.com',1)`); err != nil {
		t.Fatal(err)
	}
	for window := range 2 {
		due := at.Add(time.Duration(window) * 2 * interval)
		addAlertReports(t, db, 1, due)
		if err := q.queueAlerts(ctx, due); err != nil {
			t.Fatal(err)
		}
		m, err := claimAlert(q, due)
		if err != nil || m.To != "ops@example.com" {
			t.Fatalf("window %d: the other recipient got no digest: %v", window, err)
		}
		if err := db.CompleteMail(ctx, m, store.MailSent, "", time.Time{}, false, due); err != nil {
			t.Fatal(err)
		}
	}
	out := logs.String()
	if n := strings.Count(out, "report alert held"); n != 1 {
		t.Fatalf("%d hold messages, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "recipient=1") || !strings.Contains(out, "mail-unbounce") || strings.Contains(out, "refused@example.com") {
		t.Fatalf("hold message: %s", out)
	}
	// Clearing the address lets its next digest through.
	if removed, err := db.UnbounceMail(ctx, "refused@example.com"); err != nil || !removed {
		t.Fatal(removed, err)
	}
	due := at.Add(4 * interval)
	addAlertReports(t, db, 1, due)
	if err := q.queueAlerts(ctx, due); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT count(*) FROM mail_out WHERE kind='alert' AND to_addr='refused@example.com'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unbounced recipient got %d digests: %v", n, err)
	}
}

func TestAlertBacklogPagesAndRecipientRemoval(t *testing.T) {
	q, db := testQueue(t, Discard{})
	ctx := context.Background()
	at := time.Now()
	interval := time.Minute
	q.SetAlerts([]string{"ops@example.com"}, interval, "https://app.example.com/admin/reports")
	if err := db.InitMailAlerts(ctx, q.addresses, interval, at); err != nil {
		t.Fatal(err)
	}
	addAlertReports(t, db, 205, at)
	for i, want := range []int{200, 5} {
		due := at.Add(time.Duration(i+1) * interval)
		if err := q.queueAlerts(ctx, due); err != nil {
			t.Fatal(err)
		}
		m, err := claimAlert(q, due)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(mailText(t, m.Body), "(report "); got != want {
			t.Fatalf("page %d got %d want %d", i, got, want)
		}
		if err := db.CompleteMail(ctx, m, store.MailSent, "", time.Time{}, false, due); err != nil {
			t.Fatal(err)
		}
	}
	addAlertReports(t, db, 1, at.Add(time.Second))
	if err := q.queueAlerts(ctx, at.Add(3*interval)); err != nil {
		t.Fatal(err)
	}
	if err := db.InitMailAlerts(ctx, nil, interval, at.Add(3*interval)); err != nil {
		t.Fatal(err)
	}
	if _, err := claimAlert(q, at.Add(4*interval)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("removed recipient still sent")
	}
}

func TestAlertRetryCannotBecomeMailFlood(t *testing.T) {
	q, db := testQueue(t, sendFunc(func(context.Context, Message) error { return &SMTPError{Stage: StageRecipient, Code: 451} }))
	ctx := context.Background()
	at := time.Now()
	interval := time.Hour
	q.SetAlerts([]string{"ops@example.com"}, interval, "https://app.example.com/admin/reports")
	if err := db.InitMailAlerts(ctx, q.addresses, interval, at.Add(-interval)); err != nil {
		t.Fatal(err)
	}
	addAlertReports(t, db, 1, at)
	if err := q.queueAlerts(ctx, at); err != nil {
		t.Fatal(err)
	}
	m, err := claimAlert(q, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.deliver(ctx, m, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := claimAlert(q, at.Add(time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("retry bypassed alert interval")
	}
	addAlertReports(t, db, 1, at.Add(time.Second))
	if err := q.queueAlerts(ctx, at.Add(interval)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT count(*) FROM mail_out WHERE status='queued'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("pending retry created another digest")
	}
}

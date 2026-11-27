package mail

import (
	"context"
	"errors"
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
	if _, err := db.ClaimMail(ctx, at.Add(interval), interval); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("digest sent early")
	}
	if err := q.queueAlerts(ctx, at.Add(interval)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		m, err := db.ClaimMail(ctx, at.Add(interval), interval)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := q.decrypt(m)
		if err != nil {
			t.Fatal(err)
		}
		// Decode bodies through the MIME helper in queue tests below.
		text := mailText(t, raw)
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
	if _, err := db.ClaimMail(ctx, at.Add(interval+time.Second), interval); !errors.Is(err, store.ErrNotFound) {
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
		m, err := db.ClaimMail(ctx, at.Add(2*interval), interval)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := q.decrypt(m)
		if err != nil {
			t.Fatal(err)
		}
		text := mailText(t, raw)
		if !strings.Contains(text, next[0]) {
			t.Fatal("same-second report lost")
		}
		// First IDs include report-...-0 too; each body has exactly one listed report.
		if strings.Count(text, "(report ") != 1 {
			t.Fatalf("duplicate reports: %s", text)
		}
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
		m, err := db.ClaimMail(ctx, due, interval)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := q.decrypt(m)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(mailText(t, raw), "(report "); got != want {
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
	if _, err := db.ClaimMail(ctx, at.Add(4*interval), interval); !errors.Is(err, store.ErrNotFound) {
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
	m, err := db.ClaimMail(ctx, at, interval)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.deliver(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimMail(ctx, at.Add(time.Minute), interval); !errors.Is(err, store.ErrNotFound) {
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

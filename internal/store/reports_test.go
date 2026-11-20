package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// previousMigrations are the schema before abuse reports. When migrations
// are renumbered, change only this line.
var previousMigrations = migrations[:7]

// TestReportsMigration: a database from the previous version gets empty
// report and audit tables, and its artifacts and users are untouched.
func TestReportsMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(strings.Join(previousMigrations, ";\n") + `;
		INSERT INTO users (id, username, display_name, password_hash, role, quota_bytes, created_at) VALUES ('u', 'alice', '', '', 'user', 0, 1);
		INSERT INTO artifacts (id, slug, user_id, title, current_version, last_version, created_at, updated_at, admin_revoked) VALUES ('a', 's', 'u', 'old', 1, 1, 1, 1, 1);`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`PRAGMA user_version = ` + itoa(len(previousMigrations))); err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.DB.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("user_version %d (%v)", v, err)
	}
	if list, err := db.ListReports(ctx, "", 10); err != nil || len(list) != 0 {
		t.Fatalf("reports after migration: %v %v", list, err)
	}
	if a, err := db.ArtifactByID(ctx, "a"); err != nil || !a.AdminRevoked || a.Title != "old" {
		t.Fatalf("artifact after migration: %+v %v", a, err)
	}
	stored, err := db.AddVisitorReport(ctx, &Report{ArtifactID: "a", OwnerID: "u", Category: "spam", Network: "n"}, 10, 10)
	if err != nil || !stored {
		t.Fatalf("report after migration: %v %v", stored, err)
	}
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

func reportStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.DB.Exec(`INSERT INTO users (id, username, role, quota_bytes, created_at) VALUES ('u', 'bob', 'user', 0, 1), ('adm', 'root', 'admin', 0, 1)`); err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateArtifact(context.Background(), &NewArtifact{UserID: "u", Title: "t"}, &NewVersion{Kind: "html"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return db, id, "u"
}

// The caps hold under concurrent reports: they share one transaction with
// the insert.
func TestVisitorReportCapsUnderConcurrency(t *testing.T) {
	db, id, owner := reportStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	stored := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := db.AddVisitorReport(ctx, &Report{ArtifactID: id, OwnerID: owner, Category: "spam", Network: "net" + itoa(i)}, 100, 3)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				stored++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if n, _ := db.CountOpenReports(ctx, ReportVisitor); stored != 3 || n != 3 {
		t.Fatalf("stored %d, open %d; want 3", stored, n)
	}
	// The same network on the same artifact counts once; global cap.
	if ok, _ := db.AddVisitorReport(ctx, &Report{ArtifactID: "other", OwnerID: owner, Category: "spam", Network: "x"}, 4, 10); !ok {
		t.Fatal("report under the global cap refused")
	}
	if ok, _ := db.AddVisitorReport(ctx, &Report{ArtifactID: "other", OwnerID: owner, Category: "spam", Network: "x"}, 10, 10); ok {
		t.Fatal("duplicate from one network stored")
	}
	if ok, _ := db.AddVisitorReport(ctx, &Report{ArtifactID: "third", OwnerID: owner, Category: "spam", Network: "y"}, 4, 10); ok {
		t.Fatal("report over the global cap stored")
	}
}

func TestSystemReportsDeduplicate(t *testing.T) {
	db, id, owner := reportStore(t)
	ctx := context.Background()
	add := func(kind string, version int) bool {
		ok, err := db.AddSystemReport(ctx, &Report{ArtifactID: id, OwnerID: owner, Kind: kind, Category: "malware", Version: version})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !add("executable", 1) || add("executable", 2) || !add("form", 2) {
		t.Fatal("one open system report per artifact and kind")
	}
	list, _ := db.ListReports(ctx, ReportOpen, 10)
	for _, r := range list {
		if r.Kind == "executable" {
			if err := db.SetReportStatus(ctx, r.ID, ReportResolved, "admin"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !add("executable", 3) {
		t.Fatal("a new finding after the first was resolved was dropped")
	}
	// Visitor reports don't count against system ones, or the other way.
	if n, _ := db.CountOpenReports(ctx, ReportVisitor); n != 0 {
		t.Fatalf("visitor count %d", n)
	}
}

func TestReportStatusAudit(t *testing.T) {
	db, id, owner := reportStore(t)
	ctx := context.Background()
	r := &Report{ArtifactID: id, OwnerID: owner, Category: "spam", Network: "secret-network-key"}
	if ok, err := db.AddVisitorReport(ctx, r, 10, 10); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := db.SetReportStatus(ctx, r.ID, ReportDismissed, "root"); err != nil {
		t.Fatal(err)
	}
	got, _ := db.ReportByID(ctx, r.ID)
	if got.Status != ReportDismissed || got.ClosedBy != "root" || got.ClosedAt == nil || got.Network != "" || got.OwnerUsername != "bob" {
		t.Fatalf("dismissed: %+v", got)
	}
	if err := db.SetReportStatus(ctx, r.ID, ReportOpen, "root"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.ReportByID(ctx, r.ID)
	if got.Status != ReportOpen || got.ClosedBy != "" || got.ClosedAt != nil {
		t.Fatalf("reopened: %+v", got)
	}
	hist, _ := db.AuditFor(ctx, []string{r.ID})
	if h := hist[r.ID]; len(h) != 2 || h[0].Action != "report.dismissed" || h[1].Action != "report.open" || h[0].Actor != "root" || h[0].ArtifactID != id || h[0].UserID != owner {
		t.Fatalf("audit: %+v", h)
	}
	if err := db.SetReportStatus(ctx, "missing", ReportResolved, "root"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown report: %v", err)
	}
}

func TestSuspendReported(t *testing.T) {
	db, id, owner := reportStore(t)
	ctx := context.Background()
	second, _ := db.CreateArtifact(ctx, &NewArtifact{UserID: owner}, &NewVersion{Kind: "html"}, 0)
	mine, _ := db.CreateArtifact(ctx, &NewArtifact{UserID: "adm"}, &NewVersion{Kind: "html"}, 0)
	db.CreateSession(ctx, "h1", owner, "password", 3600e9)
	r := &Report{ArtifactID: id, OwnerID: owner, Category: "spam", Network: "a"}
	db.AddVisitorReport(ctx, r, 10, 10)
	onSecond := &Report{ArtifactID: second, OwnerID: owner, Category: "spam", Network: "b"}
	db.AddVisitorReport(ctx, onSecond, 10, 10)
	onMine := &Report{ArtifactID: mine, OwnerID: "adm", Category: "spam", Network: "c"}
	db.AddVisitorReport(ctx, onMine, 10, 10)
	before, _ := db.ArtifactByID(ctx, id)

	if _, err := db.SuspendReported(ctx, onMine.ID, "root"); !errors.Is(err, ErrOwnerIsAdmin) {
		t.Fatalf("suspending an admin: %v", err)
	}
	detail, err := db.SuspendReported(ctx, r.ID, "root")
	if err != nil || detail != "suspended the owner, revoked 2 artifacts, resolved 2 reports" {
		t.Fatalf("suspend: %q %v", detail, err)
	}
	u, _ := db.UserByID(ctx, owner)
	if !u.Disabled {
		t.Fatal("owner not disabled")
	}
	if _, err := db.SessionByHash(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("session survived the suspension")
	}
	for _, aid := range []string{id, second} {
		a, _ := db.ArtifactByID(ctx, aid)
		if !a.AdminRevoked || a.Revoked {
			t.Fatalf("artifact %s after suspension: %+v", aid, a)
		}
	}
	if a, _ := db.ArtifactByID(ctx, id); a.AccessGen == before.AccessGen {
		t.Fatal("access generation unchanged: old visit grants would still work")
	}
	if a, _ := db.ArtifactByID(ctx, mine); a.AdminRevoked {
		t.Fatal("another user's artifact revoked")
	}
	if got, _ := db.ReportByID(ctx, onMine.ID); got.Status != ReportOpen {
		t.Fatal("another user's report closed")
	}
	// A publication that commits after the suspension is revoked from birth.
	late, err := db.CreateArtifact(ctx, &NewArtifact{UserID: owner}, &NewVersion{Kind: "html"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := db.ArtifactByID(ctx, late); !a.AdminRevoked {
		t.Fatal("artifact created after the suspension is live")
	}
	hist, _ := db.AuditFor(ctx, []string{r.ID, onMine.ID})
	if len(hist[r.ID]) != 1 || hist[r.ID][0].Action != "user.suspend" || len(hist[onMine.ID]) != 0 {
		t.Fatalf("audit: %+v", hist)
	}
}

func TestRevokeReportedDeletedArtifact(t *testing.T) {
	db, id, owner := reportStore(t)
	ctx := context.Background()
	r := &Report{ArtifactID: id, OwnerID: owner, Category: "spam", Network: "a"}
	db.AddVisitorReport(ctx, r, 10, 10)
	if err := db.DeleteArtifact(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RevokeReported(ctx, r.ID, "root"); !errors.Is(err, ErrArtifactGone) {
		t.Fatalf("revoking a deleted artifact: %v", err)
	}
	// The report and its (empty) history outlive the artifact, and the
	// failed action wrote nothing.
	got, err := db.ReportByID(ctx, r.ID)
	if err != nil || got.Status != ReportOpen {
		t.Fatalf("report after delete: %+v %v", got, err)
	}
	if arts, _ := db.ArtifactsByIDs(ctx, []string{id}); len(arts) != 0 {
		t.Fatal("deleted artifact listed")
	}
	if hist, _ := db.AuditFor(ctx, []string{r.ID}); len(hist[r.ID]) != 0 {
		t.Fatalf("audit after a failed action: %+v", hist)
	}
}

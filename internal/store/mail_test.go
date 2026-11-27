package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
)

func TestMailMigrationFromV9(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(strings.Join(migrations[:9], ";\n") + `;PRAGMA user_version=9;
 INSERT INTO users(id,username,quota_bytes,created_at) VALUES('u','publisher',0,1);
 INSERT INTO onetime(id,hash,purpose,note,created_at,expires_at) VALUES('i','hash','invite','old note',1,9999999999);
 INSERT INTO artifacts(id,slug,user_id,created_at,updated_at,e2e,e2e_key_check) VALUES('a','amber-otter-quilt-river','u',1,1,1,'check');`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.ListOneTime(t.Context(), PurposeInvite)
	if err != nil || len(rows) != 1 || rows[0].Email != "" || rows[0].Note != "old note" {
		t.Fatalf("migration changed old invite: %v", err)
	}
	var e2e int
	var check string
	if err := s.DB.QueryRow(`SELECT e2e,e2e_key_check FROM artifacts WHERE id='a'`).Scan(&e2e, &check); err != nil || e2e != 1 || check != "check" {
		t.Fatal("migration changed encryption metadata")
	}
	if len(migrations) != 10 || migrations[9] != schemaV10 {
		t.Fatal("mail migration must follow v9")
	}
}

func TestConcurrentMailAdmissionAndClaim(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now()
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := &OneTime{Role: "user", Email: "to@example.com", ExpiresAt: at.Add(time.Hour)}
			m := &OutgoingMail{To: o.Email, Kind: "invite", Body: []byte("ciphertext")}
			errs <- s.CreateMailedInvite(ctx, o, ids.Hash(ids.Secret()), "same-admin", m, at)
		}()
	}
	wg.Wait()
	close(errs)
	success, limited := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrMailRate):
			limited++
		default:
			t.Fatal(err)
		}
	}
	if success != 10 || limited != 10 {
		t.Fatalf("admission %d accepted %d limited", success, limited)
	}
	claimed := make(chan string, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := s.ClaimMail(ctx, at.Add(time.Second), time.Minute)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			claimed <- m.ID
		}()
	}
	wg.Wait()
	close(claimed)
	seen := map[string]bool{}
	for id := range claimed {
		if seen[id] {
			t.Fatal("duplicate claim")
		}
		seen[id] = true
	}
	if len(seen) != 10 {
		t.Fatal("missing claim")
	}
	if _, err := s.ClaimMail(ctx, at.Add(time.Second), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatal("claimed sending row")
	}
	if err := s.RecoverMail(ctx); err != nil {
		t.Fatal(err)
	}
	m, err := s.ClaimMail(ctx, at.Add(time.Second), time.Minute)
	if err != nil || m.Attempts != 2 {
		t.Fatalf("recovery lost attempt count: %v", err)
	}
}

func TestCleanupExpiredInviteBodies(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now()
	ctx := t.Context()
	o := &OneTime{Role: "user", ExpiresAt: at.Add(time.Minute)}
	m := &OutgoingMail{To: "to@example.com", Kind: "invite", Body: []byte("ciphertext")}
	if err := s.CreateMailedInvite(ctx, o, "hash", "actor", m, at); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupMail(ctx, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	row, err := s.MailByInvite(ctx, o.ID)
	if err != nil || len(row.Body) != 0 {
		t.Fatal("expired secret retained")
	}
	if _, err := s.ClaimMail(ctx, at.Add(2*time.Minute), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired invite sent: %v", err)
	}
	row, err = s.MailByInvite(ctx, o.ID)
	if err != nil || row.Status != MailFailed {
		t.Fatal("empty claim rolled back expired-mail cancellation")
	}
	if d, err := s.MailDelay(ctx, at.Add(2*time.Minute), time.Minute); err != nil || d != time.Minute {
		t.Fatal("expired row caused a busy poll")
	}
	if err := s.ResendInviteMail(ctx, o.ID, "actor", at.Add(2*time.Minute)); !errors.Is(err, ErrLinkUsed) {
		t.Fatalf("expired invite resend: %v", err)
	}
}

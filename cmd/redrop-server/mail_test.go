package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mrRedSun/redrop/internal/store"
)

func serverMailEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for k, v := range map[string]string{"REDROP_DATA_DIR": dir, "REDROP_PUBLIC_URL": "http://app.example.com", "REDROP_CONTENT_URL": "http://content.example.com", "REDROP_SMTP_ADDR": "127.0.0.1:1", "REDROP_SMTP_TLS": "plain", "REDROP_SMTP_INSECURE_ALLOW_PLAIN": "true", "REDROP_MAIL_FROM": "share@example.com", "REDROP_NOTIFY_EMAIL": ""} {
		t.Setenv(k, v)
	}
	return dir
}
func TestOfflineInviteQueuesWithoutRunningWorker(t *testing.T) {
	dir := serverMailEnv(t)
	if err := invite([]string{"--email", "recipient@example.com", "--note", "CLI invite"}); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(dir, "redrop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	invites, err := s.ListOneTime(t.Context(), store.PurposeInvite)
	if err != nil || len(invites) != 1 {
		t.Fatalf("invite: %v", err)
	}
	row, err := s.MailByInvite(t.Context(), invites[0].ID)
	if err != nil || row.Status != store.MailQueued || row.Attempts != 0 {
		t.Fatalf("offline tool started delivery: %v", err)
	}
}
func TestMailTestValidationDoesNotOpenDatabase(t *testing.T) {
	dir := serverMailEnv(t)
	if err := mailTest(nil); err == nil {
		t.Fatal("missing --to accepted")
	}
	if err := mailTest([]string{"--to", "bad-address"}); err == nil {
		t.Fatal("invalid recipient accepted")
	}
	t.Setenv("REDROP_SMTP_ADDR", "")
	if err := mailTest([]string{"--to", "recipient@example.com"}); err == nil {
		t.Fatal("mail off accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "redrop.db")); !os.IsNotExist(err) {
		t.Fatal("mail-test opened/migrated server database")
	}
}

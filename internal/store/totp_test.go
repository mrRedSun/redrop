package store

import (
	"context"
	"path/filepath"
	"testing"
)

// UseTOTPStep accepts only steps later than the last one; a new secret
// starts over, re-saving the same secret does not.
func TestUseTOTPStep(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	u, err := createUser(ctx, db.DB, "alice", "Alice", "", "user", 0)
	if err != nil {
		t.Fatal(err)
	}
	use := func(step int64) bool {
		t.Helper()
		ok, err := db.UseTOTPStep(ctx, u.ID, step)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	db.SetTOTP(ctx, u.ID, "SECRETONE", true)
	if !use(100) || use(100) || use(99) || !use(101) {
		t.Fatal("steps are not accepted strictly in order")
	}
	db.SetTOTP(ctx, u.ID, "SECRETONE", true)
	if use(101) {
		t.Fatal("re-saving the same secret forgot the last step")
	}
	db.SetTOTP(ctx, u.ID, "SECRETTWO", true)
	if !use(50) {
		t.Fatal("a new secret kept the old secret's last step")
	}
}

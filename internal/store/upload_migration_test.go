package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestUploadBudgetMigration6FromMain5(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(schemaV1 + ";" + schemaV2 + ";" + schemaV3 + ";" + schemaV4 + ";" + schemaV5 + `;PRAGMA user_version=5;
 INSERT INTO users(id,username,quota_bytes,created_at,totp_last_step) VALUES('u','publisher',10000000,1,77);
 INSERT INTO artifacts(id,slug,user_id,created_at,updated_at,admin_revoked) VALUES('a','amber-otter-quilt-river','u',1,1,1);
 INSERT INTO uploads(id,user_id,size,received,created_at,expires_at) VALUES('up','u',64,32,unixepoch(),unixepoch()+72000);`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, revoked, step int
	var materialized, lifetime int64
	if err := db.DB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow(`SELECT admin_revoked,totp_last_step,materialized,uploads.expires_at-uploads.created_at FROM artifacts,users,uploads`).Scan(&revoked, &step, &materialized, &lifetime); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) || revoked != 1 || step != 77 || materialized != 0 || lifetime < 3600 || lifetime > 3630 {
		t.Fatalf("migration 6: version=%d admin_revoked=%d totp_step=%d materialized=%d lifetime=%d", version, revoked, step, materialized, lifetime)
	}
}

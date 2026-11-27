// Package store is redrop's SQLite persistence layer.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaV1 string

// schemaV2 adds the per-artifact info button setting. NULL follows the
// server default (REDROP_INFO_BUTTON_DEFAULT), so existing rows keep it.
const schemaV2 = `ALTER TABLE artifacts ADD COLUMN info_button INTEGER;`

// schemaV3 adds whether viewers may open older versions (off unless the owner
// enables it); a version high-water mark, so each version number is used
// once per artifact and a saved ?redrop-version=N link never shows other
// bytes; and whether a version's note may be shown to viewers. Notes written
// before viewers could see them stay private (0); new notes are public.
const schemaV3 = `ALTER TABLE artifacts ADD COLUMN viewer_old_versions INTEGER NOT NULL DEFAULT 0;
ALTER TABLE versions ADD COLUMN note_public INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artifacts ADD COLUMN last_version INTEGER NOT NULL DEFAULT 0;
UPDATE artifacts SET last_version = max(current_version, coalesce((SELECT max(number) FROM versions WHERE artifact_id = artifacts.id), 0));`

// schemaV4 records an administrator's revocation apart from the owner's, so
// an owner cannot undo it. Revocations made before it stay owner-revocable.
const schemaV4 = `ALTER TABLE artifacts ADD COLUMN admin_revoked INTEGER NOT NULL DEFAULT 0;`

// schemaV5 records the last authenticator time step accepted per user, so a
// code cannot be used twice.
const schemaV5 = `ALTER TABLE users ADD COLUMN totp_last_step INTEGER NOT NULL DEFAULT 0;`

// schemaV6 accounts for decompressed publication bytes while an upload is open.
const schemaV6 = `ALTER TABLE uploads ADD COLUMN materialized INTEGER NOT NULL DEFAULT 0;
UPDATE uploads SET expires_at = min(expires_at, unixepoch() + 3600);`

// schemaV7 adds viewer feedback (DESIGN.md "Viewer feedback"): a
// per-artifact switch, off for existing artifacts, and the comments. A
// comment keeps its version number when that version is deleted, and goes
// with its artifact.
const schemaV7 = `ALTER TABLE artifacts ADD COLUMN feedback INTEGER NOT NULL DEFAULT 0;
CREATE TABLE feedback (
    id           TEXT PRIMARY KEY,
    artifact_id  TEXT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    version      INTEGER NOT NULL,
    page         TEXT NOT NULL,
    body         TEXT NOT NULL,
    name         TEXT NOT NULL DEFAULT '',
    quote_exact  TEXT NOT NULL DEFAULT '',
    quote_prefix TEXT NOT NULL DEFAULT '',
    quote_suffix TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    resolved_at  INTEGER
);
CREATE INDEX feedback_artifact ON feedback(artifact_id, created_at);`

// schemaV8 adds abuse reports and the moderation audit log (DESIGN.md
// "Abuse handling"). A report names its artifact without a foreign key, so
// it and its history outlive the artifact.
const schemaV8 = `CREATE TABLE reports (
    id          TEXT PRIMARY KEY,
    source      TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    owner_id    TEXT NOT NULL,
    version     INTEGER NOT NULL DEFAULT 0,
    kind        TEXT NOT NULL DEFAULT '',
    url         TEXT NOT NULL DEFAULT '',
    category    TEXT NOT NULL,
    details     TEXT NOT NULL DEFAULT '',
    contact     TEXT NOT NULL DEFAULT '',
    network     TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'open',
    created_at  INTEGER NOT NULL,
    closed_at   INTEGER,
    closed_by   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX reports_status ON reports(status, created_at);
CREATE INDEX reports_artifact ON reports(artifact_id, status);
CREATE TABLE audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    at          INTEGER NOT NULL,
    actor       TEXT NOT NULL,
    action      TEXT NOT NULL,
    report_id   TEXT NOT NULL DEFAULT '',
    artifact_id TEXT NOT NULL DEFAULT '',
    user_id     TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_report ON audit(report_id);`

var migrations = []string{schemaV1, schemaV2, schemaV3, schemaV4, schemaV5, schemaV6, schemaV7, schemaV8}

var ErrNotFound = errors.New("not found")

type Store struct {
	DB *sql.DB
	// newSlug mints artifact slugs; tests replace it to force collisions.
	newSlug func() string
}

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	s := &Store{DB: db, newSlug: ids.Slug}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Tx runs fn in a transaction.
func (s *Store) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Backup writes a consistent copy of the database to path.
func (s *Store) Backup(ctx context.Context, path string) error {
	_, err := s.DB.ExecContext(ctx, "VACUUM INTO ?", path)
	return err
}

func now() int64 { return time.Now().Unix() }

func ts(v int64) time.Time { return time.Unix(v, 0).UTC() }

func tsp(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := ts(v.Int64)
	return &t
}

func nullTime(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

func nullBool(b *bool) sql.NullBool {
	if b == nil {
		return sql.NullBool{}
	}
	return sql.NullBool{Bool: *b, Valid: true}
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

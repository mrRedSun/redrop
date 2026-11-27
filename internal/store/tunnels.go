package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/pkg/api"
)

// ErrTunnelLimit is returned when a user already keeps the most tunnels
// allowed.
var ErrTunnelLimit = errors.New("tunnel limit reached")

// countTunnels is how many tunnel artifacts a user keeps, in any state.
func countTunnels(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, userID string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM artifacts a JOIN versions v ON v.artifact_id = a.id AND v.number = a.current_version
		WHERE a.user_id = ? AND v.kind = ?`, userID, api.KindTunnel).Scan(&n)
	return n, err
}

// CreateTunnel inserts a tunnel artifact (DESIGN.md "Tunnels"): an artifact
// row and its only version, of kind tunnel, with no files and no bytes. It
// reserves no storage, so it needs no upload and no quota check; instead a
// user may keep at most max tunnels. The count and the insert share one
// transaction, so concurrent creations can't exceed it.
func (s *Store) CreateTunnel(ctx context.Context, na *NewArtifact, max int) (string, error) {
	id := ids.ID()
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		kept, err := countTunnels(ctx, tx, na.UserID)
		if err != nil {
			return err
		}
		if kept >= max {
			return ErrTunnelLimit
		}
		var tok sql.NullString
		if na.TokenID != "" {
			tok = sql.NullString{String: na.TokenID, Valid: true}
		}
		n := now()
		if err := s.withNewSlug(func(slug string) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO artifacts (id, slug, user_id, token_id, token_name, title, current_version, password_hash, max_views, created_at, updated_at, expires_at, info_button, viewer_old_versions, feedback, last_version)
			VALUES (?,?,?,?,?,?,1,?,0,?,?,?,?,0,?,1)`, id, slug, na.UserID, tok, na.TokenName, na.Title, na.PasswordHash, n, n, nullTime(na.ExpiresAt), nullBool(na.InfoButton), na.Feedback)
			return err
		}); err != nil {
			return err
		}
		return insertVersion(ctx, tx, id, 1, &NewVersion{Kind: api.KindTunnel, TokenName: na.TokenName})
	})
	return id, err
}

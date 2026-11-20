package store

import (
	"context"
	"database/sql"

	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/pkg/api"
)

// CreateTunnel inserts a tunnel artifact (DESIGN.md "Tunnels"): an artifact
// row and its only version, of kind tunnel, with no files and no bytes. It
// reserves no storage, so it needs no upload and no quota check.
func (s *Store) CreateTunnel(ctx context.Context, na *NewArtifact) (string, error) {
	id := ids.ID()
	err := s.Tx(ctx, func(tx *sql.Tx) error {
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

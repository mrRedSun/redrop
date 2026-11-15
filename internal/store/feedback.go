package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
)

// Comment is one viewer comment (DESIGN.md "Viewer feedback"). It holds no
// IP address, user agent or other client detail.
type Comment struct {
	ID          string
	ArtifactID  string
	Version     int
	Page        string
	Body        string
	Name        string
	QuoteExact  string // "" = no text anchor
	QuotePrefix string
	QuoteSuffix string
	CreatedAt   time.Time
	ResolvedAt  *time.Time
}

// ErrFeedbackFull means the artifact already holds the maximum number of
// comments.
var ErrFeedbackFull = errors.New("feedback limit reached")

// AddComment stores c (its ID and creation time are set here) unless the
// artifact already has max comments. The count and the insert share one
// immediate transaction, so concurrent posts cannot pass the cap.
func (s *Store) AddComment(ctx context.Context, c *Comment, max int) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM feedback WHERE artifact_id = ?`, c.ArtifactID).Scan(&n); err != nil {
			return err
		}
		if n >= max {
			return ErrFeedbackFull
		}
		c.ID, c.CreatedAt = ids.ID(), time.Now().UTC().Truncate(time.Second)
		_, err := tx.ExecContext(ctx, `INSERT INTO feedback (id, artifact_id, version, page, body, name, quote_exact, quote_prefix, quote_suffix, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, c.ID, c.ArtifactID, c.Version, c.Page, c.Body, c.Name, c.QuoteExact, c.QuotePrefix, c.QuoteSuffix, c.CreatedAt.Unix())
		return err
	})
}

// CommentFilter selects an artifact's comments.
type CommentFilter struct {
	Version  int  // 0 = every version
	OpenOnly bool // leave out resolved comments
}

const commentCols = `id, artifact_id, version, page, body, name, quote_exact, quote_prefix, quote_suffix, created_at, resolved_at`

func scanComment(r scanner) (*Comment, error) {
	var c Comment
	var created int64
	var resolved sql.NullInt64
	if err := r.Scan(&c.ID, &c.ArtifactID, &c.Version, &c.Page, &c.Body, &c.Name, &c.QuoteExact, &c.QuotePrefix, &c.QuoteSuffix, &created, &resolved); err != nil {
		return nil, notFound(err)
	}
	c.CreatedAt, c.ResolvedAt = ts(created), tsp(resolved)
	return &c, nil
}

// ListComments returns an artifact's comments, oldest first.
func (s *Store) ListComments(ctx context.Context, artifactID string, f CommentFilter) ([]Comment, error) {
	q := `SELECT ` + commentCols + ` FROM feedback WHERE artifact_id = ?`
	args := []any{artifactID}
	if f.Version > 0 {
		q, args = q+` AND version = ?`, append(args, f.Version)
	}
	if f.OpenOnly {
		q += ` AND resolved_at IS NULL`
	}
	rows, err := s.DB.QueryContext(ctx, q+` ORDER BY created_at, rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// SetCommentResolved marks a comment of the artifact resolved (now) or open
// again, and returns it. A comment of another artifact is ErrNotFound.
func (s *Store) SetCommentResolved(ctx context.Context, artifactID, id string, resolved bool) (*Comment, error) {
	at := sql.NullInt64{}
	if resolved {
		at = sql.NullInt64{Int64: now(), Valid: true}
	}
	// Resolving twice keeps the first time.
	res, err := s.DB.ExecContext(ctx, `UPDATE feedback SET resolved_at = CASE WHEN ? THEN coalesce(resolved_at, ?) END WHERE artifact_id = ? AND id = ?`,
		resolved, at, artifactID, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return scanComment(s.DB.QueryRowContext(ctx, `SELECT `+commentCols+` FROM feedback WHERE artifact_id = ? AND id = ?`, artifactID, id))
}

// DeleteComment deletes one comment of the artifact.
func (s *Store) DeleteComment(ctx context.Context, artifactID, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM feedback WHERE artifact_id = ? AND id = ?`, artifactID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

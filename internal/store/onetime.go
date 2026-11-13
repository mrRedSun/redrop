package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
)

const (
	PurposeInvite = "invite"
	PurposeReset  = "reset"
	PurposeSetup  = "setup"
)

type OneTime struct {
	ID         string
	Purpose    string
	UserID     string
	CreatedBy  string
	Role       string
	QuotaBytes int64
	ClearTOTP  bool
	Note       string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	UsedAt     *time.Time
	UsedBy     string
	UsedByName string
}

var ErrLinkUsed = errors.New("link already used or expired")

func (s *Store) CreateOneTime(ctx context.Context, o *OneTime, hash string) error {
	if o.ID == "" {
		o.ID = ids.ID()
	}
	o.CreatedAt = time.Now().UTC().Truncate(time.Second)
	var uid, by sql.NullString
	if o.UserID != "" {
		uid = sql.NullString{String: o.UserID, Valid: true}
	}
	if o.CreatedBy != "" {
		by = sql.NullString{String: o.CreatedBy, Valid: true}
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO onetime (id, hash, purpose, user_id, created_by, role, quota_bytes, clear_totp, note, created_at, expires_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		o.ID, hash, o.Purpose, uid, by, o.Role, o.QuotaBytes, o.ClearTOTP, o.Note, o.CreatedAt.Unix(), o.ExpiresAt.Unix())
	return err
}

const oneTimeCols = `o.id, o.purpose, coalesce(o.user_id,''), coalesce(o.created_by,''), o.role, o.quota_bytes, o.clear_totp, o.note, o.created_at, o.expires_at, o.used_at, coalesce(o.used_by,''), coalesce(u.username,'')`

func scanOneTime(r scanner) (*OneTime, error) {
	var o OneTime
	var created, exp int64
	var used sql.NullInt64
	if err := r.Scan(&o.ID, &o.Purpose, &o.UserID, &o.CreatedBy, &o.Role, &o.QuotaBytes, &o.ClearTOTP, &o.Note, &created, &exp, &used, &o.UsedBy, &o.UsedByName); err != nil {
		return nil, notFound(err)
	}
	o.CreatedAt, o.ExpiresAt, o.UsedAt = ts(created), ts(exp), tsp(used)
	return &o, nil
}

// OneTimeByHash returns an unused, unexpired link.
func (s *Store) OneTimeByHash(ctx context.Context, purpose, hash string) (*OneTime, error) {
	return scanOneTime(s.DB.QueryRowContext(ctx, `SELECT `+oneTimeCols+` FROM onetime o LEFT JOIN users u ON u.id = o.used_by
		WHERE o.hash = ? AND o.purpose = ? AND o.used_at IS NULL AND o.expires_at > ?`, hash, purpose, now()))
}

func (s *Store) ListOneTime(ctx context.Context, purpose string) ([]*OneTime, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+oneTimeCols+` FROM onetime o LEFT JOIN users u ON u.id = o.used_by WHERE o.purpose = ? ORDER BY o.created_at DESC`, purpose)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OneTime
	for rows.Next() {
		o, err := scanOneTime(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) DeleteOneTime(ctx context.Context, purpose, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM onetime WHERE id = ? AND purpose = ?`, id, purpose)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// consume atomically marks the link used inside tx.
func consume(ctx context.Context, tx *sql.Tx, id, userID string) error {
	res, err := tx.ExecContext(ctx, `UPDATE onetime SET used_at = ?, used_by = ? WHERE id = ? AND used_at IS NULL AND expires_at > ?`, now(), userID, id, now())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLinkUsed
	}
	return nil
}

// RedeemAccountLink consumes an invite/setup link and creates the user,
// optionally linking an OIDC identity, in one transaction.
func (s *Store) RedeemAccountLink(ctx context.Context, o *OneTime, username, displayName, passwordHash string, defaultQuota int64, provider, subject, email string) (*User, error) {
	var u *User
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		quota := o.QuotaBytes
		if quota <= 0 {
			quota = defaultQuota
		}
		role := o.Role
		if o.Purpose == PurposeSetup {
			role = "admin"
		}
		var err error
		if u, err = createUser(ctx, tx, username, displayName, passwordHash, role, quota); err != nil {
			return err
		}
		if provider != "" {
			if err := linkIdentity(ctx, tx, u.ID, provider, subject, email); err != nil {
				return err
			}
		}
		return consume(ctx, tx, o.ID, u.ID)
	})
	return u, err
}

// RedeemReset consumes a reset link and sets the new password.
func (s *Store) RedeemReset(ctx context.Context, o *OneTime, passwordHash string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if err := consume(ctx, tx, o.ID, o.UserID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, o.UserID); err != nil {
			return err
		}
		if o.ClearTOTP {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET totp_secret = '', totp_enabled = 0 WHERE id = ?`, o.UserID); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, o.UserID)
		return err
	})
}

// HasOpenSetup reports whether an unused setup link exists.
func (s *Store) DeleteOpenSetupLinks(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM onetime WHERE purpose = 'setup' AND used_at IS NULL`)
	return err
}

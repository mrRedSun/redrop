package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
)

type User struct {
	ID           string
	Username     string
	DisplayName  string
	PasswordHash string
	TOTPSecret   string
	TOTPEnabled  bool
	Role         string
	QuotaBytes   int64
	Disabled     bool
	CreatedAt    time.Time
}

const userCols = `id, username, display_name, password_hash, totp_secret, totp_enabled, role, quota_bytes, disabled, created_at`

type scanner interface{ Scan(...any) error }

// scanUser reads userCols, followed by any extra selected columns.
func scanUser(r scanner, extra ...any) (*User, error) {
	var u User
	var created int64
	dest := append([]any{&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.TOTPSecret, &u.TOTPEnabled, &u.Role, &u.QuotaBytes, &u.Disabled, &created}, extra...)
	if err := r.Scan(dest...); err != nil {
		return nil, notFound(err)
	}
	u.CreatedAt = ts(created)
	return &u, nil
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func createUser(ctx context.Context, q execer, username, displayName, passwordHash, role string, quota int64) (*User, error) {
	u := &User{ID: ids.ID(), Username: username, DisplayName: displayName, PasswordHash: passwordHash, Role: role, QuotaBytes: quota, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	_, err := q.ExecContext(ctx, `INSERT INTO users (id, username, display_name, password_hash, role, quota_bytes, created_at) VALUES (?,?,?,?,?,?,?)`,
		u.ID, u.Username, u.DisplayName, u.PasswordHash, u.Role, u.QuotaBytes, u.CreatedAt.Unix())
	if err != nil {
		return nil, err
	}
	return u, nil
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, username))
}

func (s *Store) UsernameTaken(ctx context.Context, username string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE username = ?`, username).Scan(&n)
	return n > 0, err
}

func (s *Store) SetPassword(ctx context.Context, userID, hash string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
	return err
}

func (s *Store) SetTOTP(ctx context.Context, userID, secret string, enabled bool) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET totp_secret = ?, totp_enabled = ? WHERE id = ?`, secret, enabled, userID)
	return err
}

func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET display_name = ?, role = ?, quota_bytes = ?, disabled = ? WHERE id = ?`,
		u.DisplayName, u.Role, u.QuotaBytes, u.Disabled, u.ID)
	return err
}

func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

// UsedBytes is the logical size of all retained versions owned by a user.
func (s *Store) UsedBytes(ctx context.Context, userID string) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, `SELECT coalesce(sum(v.size),0) FROM versions v JOIN artifacts a ON a.id = v.artifact_id WHERE a.user_id = ?`, userID).Scan(&n)
	return n, err
}

// Usage is a user's storage use and artifact count.
type Usage struct {
	UsedBytes int64
	Artifacts int
}

// UserWithUsage pairs a user with their usage.
type UserWithUsage struct {
	*User
	Usage
}

// usageJoin aggregates bytes and artifact counts per user in two subqueries,
// so artifacts are never counted once per version.
const usageJoin = ` LEFT JOIN (SELECT a.user_id, sum(v.size) AS used FROM versions v JOIN artifacts a ON a.id = v.artifact_id GROUP BY a.user_id) ub ON ub.user_id = users.id
	LEFT JOIN (SELECT user_id, count(*) AS n FROM artifacts GROUP BY user_id) ac ON ac.user_id = users.id`

// UserUsage returns one user's usage.
func (s *Store) UserUsage(ctx context.Context, userID string) (Usage, error) {
	var u Usage
	err := s.DB.QueryRowContext(ctx, `SELECT coalesce(ub.used,0), coalesce(ac.n,0) FROM users`+usageJoin+` WHERE users.id = ?`, userID).Scan(&u.UsedBytes, &u.Artifacts)
	return u, notFound(err)
}

// ListUsersWithUsage returns every user with usage in one query.
func (s *Store) ListUsersWithUsage(ctx context.Context) ([]UserWithUsage, error) {
	cols := strings.ReplaceAll("users."+userCols, ", ", ", users.")
	rows, err := s.DB.QueryContext(ctx, `SELECT `+cols+`, coalesce(ub.used,0), coalesce(ac.n,0) FROM users`+usageJoin+` ORDER BY users.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserWithUsage
	for rows.Next() {
		var usage Usage
		u, err := scanUser(rows, &usage.UsedBytes, &usage.Artifacts)
		if err != nil {
			return nil, err
		}
		out = append(out, UserWithUsage{User: u, Usage: usage})
	}
	return out, rows.Err()
}

// --- identities ---

type Identity struct {
	ID        string
	UserID    string
	Provider  string
	Subject   string
	Email     string
	CreatedAt time.Time
}

func (s *Store) IdentityBySubject(ctx context.Context, provider, subject string) (*Identity, error) {
	var i Identity
	var created int64
	err := s.DB.QueryRowContext(ctx, `SELECT id, user_id, provider, subject, email, created_at FROM identities WHERE provider = ? AND subject = ?`, provider, subject).
		Scan(&i.ID, &i.UserID, &i.Provider, &i.Subject, &i.Email, &created)
	if err != nil {
		return nil, notFound(err)
	}
	i.CreatedAt = ts(created)
	return &i, nil
}

func (s *Store) LinkIdentity(ctx context.Context, userID, provider, subject, email string) error {
	return linkIdentity(ctx, s.DB, userID, provider, subject, email)
}

func linkIdentity(ctx context.Context, q execer, userID, provider, subject, email string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO identities (id, user_id, provider, subject, email, created_at) VALUES (?,?,?,?,?,?)`,
		ids.ID(), userID, provider, subject, email, now())
	return err
}

func (s *Store) ListIdentities(ctx context.Context, userID string) ([]Identity, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, user_id, provider, subject, email, created_at FROM identities WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var i Identity
		var created int64
		if err := rows.Scan(&i.ID, &i.UserID, &i.Provider, &i.Subject, &i.Email, &created); err != nil {
			return nil, err
		}
		i.CreatedAt = ts(created)
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) UnlinkIdentity(ctx context.Context, userID, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM identities WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- sessions ---

type Session struct {
	UserID    string
	Method    string
	ExpiresAt time.Time
}

func (s *Store) CreateSession(ctx context.Context, hash, userID, method string, ttl time.Duration) error {
	n := now()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sessions (hash, user_id, method, created_at, last_seen_at, expires_at) VALUES (?,?,?,?,?,?)`,
		hash, userID, method, n, n, n+int64(ttl.Seconds()))
	return err
}

func (s *Store) SessionByHash(ctx context.Context, hash string) (*Session, error) {
	var se Session
	var exp int64
	err := s.DB.QueryRowContext(ctx, `SELECT user_id, method, expires_at FROM sessions WHERE hash = ? AND expires_at > ?`, hash, now()).Scan(&se.UserID, &se.Method, &exp)
	if err != nil {
		return nil, notFound(err)
	}
	se.ExpiresAt = ts(exp)
	return &se, nil
}

func (s *Store) TouchSession(ctx context.Context, hash string) {
	s.DB.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE hash = ? AND last_seen_at < ?`, now(), hash, now()-300)
}

func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE hash = ?`, hash)
	return err
}

// DeleteUserSessions removes all sessions of a user except keepHash.
func (s *Store) DeleteUserSessions(ctx context.Context, userID, keepHash string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND hash != ?`, userID, keepHash)
	return err
}

// --- tokens ---

type Token struct {
	ID         string
	UserID     string
	Name       string
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	Artifacts  int
}

func (s *Store) CreateToken(ctx context.Context, userID, name, prefix, hash string, expires *time.Time) (*Token, error) {
	t := &Token{ID: ids.ID(), UserID: userID, Name: name, Prefix: prefix, CreatedAt: time.Now().UTC().Truncate(time.Second), ExpiresAt: expires}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO tokens (id, user_id, name, prefix, hash, created_at, expires_at) VALUES (?,?,?,?,?,?,?)`,
		t.ID, userID, name, prefix, hash, t.CreatedAt.Unix(), nullTime(expires))
	return t, err
}

func (s *Store) TokenByHash(ctx context.Context, hash string) (*Token, error) {
	var t Token
	var created int64
	var last, exp sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT id, user_id, name, prefix, created_at, last_used_at, expires_at FROM tokens WHERE hash = ?`, hash).
		Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &created, &last, &exp)
	if err != nil {
		return nil, notFound(err)
	}
	t.CreatedAt, t.LastUsedAt, t.ExpiresAt = ts(created), tsp(last), tsp(exp)
	if t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
		return nil, ErrNotFound
	}
	return &t, nil
}

func (s *Store) TouchToken(ctx context.Context, id string) {
	s.DB.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ? AND coalesce(last_used_at,0) < ?`, now(), id, now()-60)
}

func (s *Store) ListTokens(ctx context.Context, userID string) ([]Token, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT t.id, t.user_id, t.name, t.prefix, t.created_at, t.last_used_at, t.expires_at,
		(SELECT count(*) FROM artifacts a WHERE a.token_id = t.id)
		FROM tokens t WHERE t.user_id = ? ORDER BY t.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var created int64
		var last, exp sql.NullInt64
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &created, &last, &exp, &t.Artifacts); err != nil {
			return nil, err
		}
		t.CreatedAt, t.LastUsedAt, t.ExpiresAt = ts(created), tsp(last), tsp(exp)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteToken(ctx context.Context, userID, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM tokens WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

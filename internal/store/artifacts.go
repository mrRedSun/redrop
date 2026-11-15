package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/pkg/api"
)

type Artifact struct {
	ID             string
	Slug           string
	UserID         string
	OwnerUsername  string
	TokenID        string
	TokenName      string
	Title          string
	CurrentVersion int
	PasswordHash   string
	MaxViews       int
	Views          int
	Previews       int
	AccessGen      int
	Revoked        bool // by the owner
	AdminRevoked   bool // by an administrator; only an administrator clears it
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ExpiresAt      *time.Time
	ExhaustedAt    *time.Time
	LastViewedAt   *time.Time
	InfoButton     *bool // nil = server default
	OldVersions    bool  // viewers may open older versions
	Feedback       bool  // viewers may leave comments
	LastVersion    int   // highest version number ever used

	// Current version details (joined).
	Kind      string
	Published time.Time // when the current version was published
	Size      int64
	Files     int
	Entry     string
	SPA       bool
	TotalSize int64
	Versions  int
}

// Status computes the lifecycle state at time t.
func (a *Artifact) Status(t time.Time) string {
	switch {
	case a.Revoked || a.AdminRevoked:
		return api.StatusRevoked
	case a.ExpiresAt != nil && !t.Before(*a.ExpiresAt):
		return api.StatusExpired
	case a.MaxViews > 0 && a.Views >= a.MaxViews:
		return api.StatusExhausted
	}
	return api.StatusActive
}

type File struct {
	Path        string
	Blob        string
	Size        int64
	ContentType string
}

type NewVersion struct {
	UploadID  string // reservation converted to committed bytes in the same transaction
	Kind      string
	Entry     string
	SPA       bool
	Note      string
	TokenName string
	Files     []File
}

func (v *NewVersion) size() int64 {
	var n int64
	for _, f := range v.Files {
		n += f.Size
	}
	return n
}

const artifactSelect = `SELECT a.id, a.slug, a.user_id, u.username, coalesce(a.token_id,''), a.token_name, a.title, a.current_version, a.password_hash,
	a.max_views, a.views, a.previews, a.access_gen, a.revoked, a.admin_revoked, a.created_at, a.updated_at, a.expires_at, a.exhausted_at, a.last_viewed_at, a.info_button, a.viewer_old_versions, a.feedback, a.last_version,
	coalesce(v.kind,''), coalesce(v.size,0), coalesce(v.files,0), coalesce(v.entry,''), coalesce(v.spa,0), coalesce(v.created_at,0),
	(SELECT coalesce(sum(size),0) FROM versions WHERE artifact_id = a.id),
	(SELECT count(*) FROM versions WHERE artifact_id = a.id)
	FROM artifacts a JOIN users u ON u.id = a.user_id
	LEFT JOIN versions v ON v.artifact_id = a.id AND v.number = a.current_version`

func scanArtifact(r scanner) (*Artifact, error) {
	var a Artifact
	var created, updated int64
	var published int64
	var exp, exh, last sql.NullInt64
	var info sql.NullBool
	err := r.Scan(&a.ID, &a.Slug, &a.UserID, &a.OwnerUsername, &a.TokenID, &a.TokenName, &a.Title, &a.CurrentVersion, &a.PasswordHash,
		&a.MaxViews, &a.Views, &a.Previews, &a.AccessGen, &a.Revoked, &a.AdminRevoked, &created, &updated, &exp, &exh, &last, &info, &a.OldVersions, &a.Feedback, &a.LastVersion,
		&a.Kind, &a.Size, &a.Files, &a.Entry, &a.SPA, &published, &a.TotalSize, &a.Versions)
	if err != nil {
		return nil, notFound(err)
	}
	if info.Valid {
		a.InfoButton = &info.Bool
	}
	a.CreatedAt, a.UpdatedAt, a.Published = ts(created), ts(updated), ts(published)
	a.ExpiresAt, a.ExhaustedAt, a.LastViewedAt = tsp(exp), tsp(exh), tsp(last)
	return &a, nil
}

func (s *Store) ArtifactByID(ctx context.Context, id string) (*Artifact, error) {
	return scanArtifact(s.DB.QueryRowContext(ctx, artifactSelect+` WHERE a.id = ?`, id))
}

func (s *Store) ArtifactBySlug(ctx context.Context, slug string) (*Artifact, error) {
	return scanArtifact(s.DB.QueryRowContext(ctx, artifactSelect+` WHERE a.slug = ?`, slug))
}

type ListFilter struct {
	UserID string // empty = all users
	Query  string
	Limit  int
}

func (s *Store) ListArtifacts(ctx context.Context, f ListFilter) ([]*Artifact, error) {
	q := artifactSelect + ` WHERE 1=1`
	var args []any
	if f.UserID != "" {
		q += ` AND a.user_id = ?`
		args = append(args, f.UserID)
	}
	if f.Query != "" {
		q += ` AND (a.title LIKE ? ESCAPE '\' OR a.id = ? OR a.token_name LIKE ? ESCAPE '\')`
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.Query) + "%"
		args = append(args, like, f.Query, like)
	}
	q += ` ORDER BY a.updated_at DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func insertVersion(ctx context.Context, tx *sql.Tx, artifactID string, number int, v *NewVersion) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO versions (artifact_id, number, kind, size, files, entry, spa, note, note_public, token_name, created_at) VALUES (?,?,?,?,?,?,?,?,1,?,?)`,
		artifactID, number, v.Kind, v.size(), len(v.Files), v.Entry, v.SPA, v.Note, v.TokenName, now()); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO files (artifact_id, version, path, blob, size, content_type) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, f := range v.Files {
		if _, err := stmt.ExecContext(ctx, artifactID, number, f.Path, f.Blob, f.Size, f.ContentType); err != nil {
			return err
		}
	}
	return nil
}

type NewArtifact struct {
	UserID       string
	TokenID      string
	TokenName    string
	Title        string
	PasswordHash string
	MaxViews     int
	ExpiresAt    *time.Time
	InfoButton   *bool // nil = server default
	OldVersions  bool
	Feedback     bool
}

// ErrQuota is returned when a write would exceed a quota.
var ErrQuota = errors.New("quota exceeded")

// checkQuota verifies the user quota and the server total inside tx for a
// pending change. User usage counts each open upload once, as the larger of
// its staged and unpacked bytes, because both hold the same content; userBytes
// replaces the usage of the upload named by replacing ("" for none). The
// server total counts staged and unpacked bytes separately, because both are
// on disk until the staged file is removed, and grows by totalAdd.
func checkQuota(ctx context.Context, tx *sql.Tx, userID, replacing string, userBytes, totalAdd, maxTotal int64) error {
	var used, quota, total int64
	if err := tx.QueryRowContext(ctx, `SELECT quota_bytes FROM users WHERE id = ?`, userID).Scan(&quota); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT coalesce(sum(v.size),0) FROM versions v JOIN artifacts a ON a.id = v.artifact_id WHERE a.user_id = ?) + (SELECT coalesce(sum(max(size, materialized)),0) FROM uploads WHERE user_id = ? AND id != ?)`, userID, userID, replacing).Scan(&used); err != nil {
		return err
	}
	if quota > 0 && used+userBytes > quota {
		return ErrQuota
	}
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT coalesce(sum(size),0) FROM versions) + (SELECT coalesce(sum(size + materialized),0) FROM uploads)`).Scan(&total); err != nil {
		return err
	}
	if maxTotal > 0 && total+totalAdd > maxTotal {
		return ErrQuota
	}
	return nil
}

func (s *Store) CreateArtifact(ctx context.Context, na *NewArtifact, v *NewVersion, maxTotal int64) (string, error) {
	id := ids.ID()
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := finishMaterialization(ctx, tx, na.UserID, v.UploadID); err != nil {
			return err
		}
		if err := checkQuota(ctx, tx, na.UserID, v.UploadID, v.size(), v.size(), maxTotal); err != nil {
			return err
		}
		var tok sql.NullString
		if na.TokenID != "" {
			tok = sql.NullString{String: na.TokenID, Valid: true}
		}
		n := now()
		if err := s.withNewSlug(func(slug string) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO artifacts (id, slug, user_id, token_id, token_name, title, current_version, password_hash, max_views, created_at, updated_at, expires_at, info_button, viewer_old_versions, feedback, last_version)
			VALUES (?,?,?,?,?,?,1,?,?,?,?,?,?,?,?,1)`, id, slug, na.UserID, tok, na.TokenName, na.Title, na.PasswordHash, na.MaxViews, n, n, nullTime(na.ExpiresAt), nullBool(na.InfoButton), na.OldVersions, na.Feedback)
			return err
		}); err != nil {
			return err
		}
		return insertVersion(ctx, tx, id, 1, v)
	})
	return id, err
}

// AddVersion appends a version and makes it current. keep > 0 prunes older versions.
func (s *Store) AddVersion(ctx context.Context, artifactID, userID string, v *NewVersion, maxTotal int64, keep int) (int, error) {
	var number int
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := finishMaterialization(ctx, tx, userID, v.UploadID); err != nil {
			return err
		}
		if err := checkQuota(ctx, tx, userID, v.UploadID, v.size(), v.size(), maxTotal); err != nil {
			return err
		}
		// Never reuse a number, even one whose version was deleted.
		if err := tx.QueryRowContext(ctx, `SELECT max(last_version, coalesce((SELECT max(number) FROM versions WHERE artifact_id = ?), 0)) + 1 FROM artifacts WHERE id = ?`, artifactID, artifactID).Scan(&number); err != nil {
			return notFound(err)
		}
		if err := insertVersion(ctx, tx, artifactID, number, v); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE artifacts SET current_version = ?, last_version = ?, updated_at = ? WHERE id = ?`, number, number, now(), artifactID); err != nil {
			return err
		}
		if keep > 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM versions WHERE artifact_id = ? AND number <= ?`, artifactID, number-keep); err != nil {
				return err
			}
		}
		return nil
	})
	return number, err
}

func (s *Store) SetCurrentVersion(ctx context.Context, artifactID string, number int) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE artifacts SET current_version = ?, updated_at = ? WHERE id = ? AND EXISTS (SELECT 1 FROM versions WHERE artifact_id = ? AND number = ?)`,
		number, now(), artifactID, artifactID, number)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ArtifactUpdate applies non-nil fields.
type ArtifactUpdate struct {
	Title        *string
	ExpiresAt    **time.Time
	PasswordHash *string
	MaxViews     *int
	ResetViews   bool
	Revoked      *bool
	AdminRevoked *bool
	RotateSlug   bool
	BumpGen      bool
	SPA          *bool
	InfoButton   *bool
	OldVersions  *bool
	Feedback     *bool
}

func (s *Store) UpdateArtifact(ctx context.Context, id string, u ArtifactUpdate) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		set := []string{"updated_at = ?"}
		args := []any{now()}
		if u.Title != nil {
			set, args = append(set, "title = ?"), append(args, *u.Title)
		}
		if u.ExpiresAt != nil {
			set, args = append(set, "expires_at = ?"), append(args, nullTime(*u.ExpiresAt))
		}
		if u.PasswordHash != nil {
			set, args = append(set, "password_hash = ?"), append(args, *u.PasswordHash)
		}
		if u.MaxViews != nil {
			set, args = append(set, "max_views = ?"), append(args, *u.MaxViews)
		}
		if u.ResetViews {
			set = append(set, "views = 0")
		}
		if u.MaxViews != nil || u.ResetViews {
			// Re-evaluate exhaustion after limits change.
			set = append(set, "exhausted_at = NULL")
		}
		if u.Revoked != nil {
			set, args = append(set, "revoked = ?"), append(args, *u.Revoked)
		}
		if u.AdminRevoked != nil {
			set, args = append(set, "admin_revoked = ?"), append(args, *u.AdminRevoked)
		}
		if u.InfoButton != nil {
			set, args = append(set, "info_button = ?"), append(args, *u.InfoButton)
		}
		if u.OldVersions != nil {
			set, args = append(set, "viewer_old_versions = ?"), append(args, *u.OldVersions)
		}
		if u.Feedback != nil {
			set, args = append(set, "feedback = ?"), append(args, *u.Feedback)
		}
		slugArg := -1
		if u.RotateSlug {
			set, args, slugArg = append(set, "slug = ?"), append(args, ""), len(args)
		}
		if u.BumpGen || u.RotateSlug || u.PasswordHash != nil || (u.Revoked != nil && *u.Revoked) || (u.AdminRevoked != nil && *u.AdminRevoked) {
			set = append(set, "access_gen = access_gen + 1")
		}
		args = append(args, id)
		var res sql.Result
		update := func() (err error) {
			res, err = tx.ExecContext(ctx, `UPDATE artifacts SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...)
			return err
		}
		var err error
		if slugArg >= 0 {
			err = s.withNewSlug(func(slug string) error { args[slugArg] = slug; return update() })
		} else {
			err = update()
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if u.SPA != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE versions SET spa = ? WHERE artifact_id = ? AND number = (SELECT current_version FROM artifacts WHERE id = ?)`, *u.SPA, id, id); err != nil {
				return err
			}
		}
		// Mark exhausted immediately if new limit already reached.
		_, err = tx.ExecContext(ctx, `UPDATE artifacts SET exhausted_at = ? WHERE id = ? AND max_views > 0 AND views >= max_views AND exhausted_at IS NULL`, now(), id)
		return err
	})
}

func (s *Store) DeleteArtifact(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM artifacts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type VersionRow struct {
	Number     int
	Kind       string
	Size       int64
	Files      int
	Entry      string
	SPA        bool
	Note       string
	NotePublic bool // shown to viewers; false for notes written before they could be
	TokenName  string
	CreatedAt  time.Time
}

// VersionAt returns one version's row, or ErrNotFound when it is not retained.
func (s *Store) VersionAt(ctx context.Context, artifactID string, number int) (VersionRow, error) {
	var v VersionRow
	var created int64
	err := s.DB.QueryRowContext(ctx, `SELECT number, kind, size, files, entry, spa, note, note_public, token_name, created_at FROM versions WHERE artifact_id = ? AND number = ?`, artifactID, number).
		Scan(&v.Number, &v.Kind, &v.Size, &v.Files, &v.Entry, &v.SPA, &v.Note, &v.NotePublic, &v.TokenName, &created)
	if err != nil {
		return VersionRow{}, notFound(err)
	}
	v.CreatedAt = ts(created)
	return v, nil
}

func (s *Store) ListVersions(ctx context.Context, artifactID string) ([]VersionRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT number, kind, size, files, entry, spa, note, note_public, token_name, created_at FROM versions WHERE artifact_id = ? ORDER BY number DESC`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VersionRow
	for rows.Next() {
		var v VersionRow
		var created int64
		if err := rows.Scan(&v.Number, &v.Kind, &v.Size, &v.Files, &v.Entry, &v.SPA, &v.Note, &v.NotePublic, &v.TokenName, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = ts(created)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) DeleteVersion(ctx context.Context, artifactID string, number int) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM versions WHERE artifact_id = ? AND number = ? AND number != (SELECT current_version FROM artifacts WHERE id = ?)`, artifactID, number, artifactID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListFiles(ctx context.Context, artifactID string, version int) ([]File, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT path, blob, size, content_type FROM files WHERE artifact_id = ? AND version = ? ORDER BY path`, artifactID, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Path, &f.Blob, &f.Size, &f.ContentType); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) FileAt(ctx context.Context, artifactID string, version int, path string) (*File, error) {
	var f File
	err := s.DB.QueryRowContext(ctx, `SELECT path, blob, size, content_type FROM files WHERE artifact_id = ? AND version = ? AND path = ?`, artifactID, version, path).
		Scan(&f.Path, &f.Blob, &f.Size, &f.ContentType)
	if err != nil {
		return nil, notFound(err)
	}
	return &f, nil
}

// HasPrefix reports whether any file lives under dir/ (for directory listing).
func (s *Store) DirFiles(ctx context.Context, artifactID string, version int, dir string) ([]File, error) {
	like := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(dir) + "%"
	rows, err := s.DB.QueryContext(ctx, `SELECT path, blob, size, content_type FROM files WHERE artifact_id = ? AND version = ? AND path LIKE ? ESCAPE '\' ORDER BY path LIMIT 5000`, artifactID, version, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Path, &f.Blob, &f.Size, &f.ContentType); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// --- views ---

const (
	OutcomeView        = "view"
	OutcomePreview     = "preview"
	OutcomeDenied      = "denied"
	OutcomeBadPassword = "bad_password"
)

func (s *Store) RecordView(ctx context.Context, artifactID, outcome, country, client string) {
	n := now()
	s.DB.ExecContext(ctx, `INSERT INTO views (artifact_id, at, outcome, country, client) VALUES (?,?,?,?,?)`, artifactID, n, outcome, country, client)
	switch outcome {
	case OutcomeView:
		s.DB.ExecContext(ctx, `UPDATE artifacts SET views = views + 1, last_viewed_at = ? WHERE id = ?`, n, artifactID)
	case OutcomePreview:
		s.DB.ExecContext(ctx, `UPDATE artifacts SET previews = previews + 1 WHERE id = ?`, artifactID)
	}
}

// ConsumeView atomically counts a view on a view-limited artifact; it returns
// false when no views remain.
func (s *Store) ConsumeView(ctx context.Context, artifactID, country, client string) (bool, error) {
	ok := false
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		n := now()
		res, err := tx.ExecContext(ctx, `UPDATE artifacts SET views = views + 1, last_viewed_at = ? WHERE id = ? AND (max_views = 0 OR views < max_views)`, n, artifactID)
		if err != nil {
			return err
		}
		if c, _ := res.RowsAffected(); c == 0 {
			return nil
		}
		ok = true
		if _, err := tx.ExecContext(ctx, `UPDATE artifacts SET exhausted_at = ? WHERE id = ? AND max_views > 0 AND views >= max_views AND exhausted_at IS NULL`, n, artifactID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO views (artifact_id, at, outcome, country, client) VALUES (?,?,?,?,?)`, artifactID, n, OutcomeView, country, client)
		return err
	})
	return ok, err
}

type ViewRow struct {
	At      time.Time
	Outcome string
	Country string
	Client  string
}

func (s *Store) ListViews(ctx context.Context, artifactID string, limit int) ([]ViewRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT at, outcome, country, client FROM views WHERE artifact_id = ? ORDER BY at DESC, rowid DESC LIMIT ?`, artifactID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ViewRow
	for rows.Next() {
		var v ViewRow
		var at int64
		if err := rows.Scan(&at, &v.Outcome, &v.Country, &v.Client); err != nil {
			return nil, err
		}
		v.At = ts(at)
		out = append(out, v)
	}
	return out, rows.Err()
}

// RecentFailures counts bad-password attempts for an artifact since t.
func (s *Store) RecentFailures(ctx context.Context, artifactID string, since time.Time) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM views WHERE artifact_id = ? AND outcome = 'bad_password' AND at >= ?`, artifactID, since.Unix()).Scan(&n)
	return n, err
}

// --- uploads ---

type Upload struct {
	ID           string
	UserID       string
	Size         int64
	Received     int64
	Materialized int64 // private in-flight or failed-cleanup reservation
	ExpiresAt    time.Time
}

var ErrUploadLimit = errors.New("too many open uploads")

func (s *Store) CreateUpload(ctx context.Context, userID string, size int64, ttl time.Duration, maxTotal int64, maxOpen int) (*Upload, error) {
	n := now()
	u := &Upload{ID: ids.Random(16), UserID: userID, Size: size, ExpiresAt: ts(n + int64(ttl/time.Second))}
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM uploads WHERE user_id = ?`, userID).Scan(&count); err != nil {
			return err
		}
		if count >= maxOpen {
			return ErrUploadLimit
		}
		if err := checkQuota(ctx, tx, userID, "", size, size, maxTotal); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO uploads (id, user_id, size, created_at, expires_at) VALUES (?,?,?,?,?)`, u.ID, userID, size, n, u.ExpiresAt.Unix())
		return err
	})
	return u, err
}

// ReserveMaterialization reserves each file's declared unpacked size before any
// blob bytes are written. Upload bytes remain reserved until safely removed.
func (s *Store) ReserveMaterialization(ctx context.Context, userID, id string, size, maxTotal int64) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var usage int64 // the upload's user usage after this reservation
		if err := tx.QueryRowContext(ctx, `SELECT max(size, materialized + ?) FROM uploads WHERE id = ? AND user_id = ? AND expires_at > ?`, size, id, userID, now()).Scan(&usage); err != nil {
			return notFound(err)
		}
		if err := checkQuota(ctx, tx, userID, id, usage, size, maxTotal); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE uploads SET materialized = materialized + ? WHERE id = ? AND user_id = ? AND expires_at > ?`, size, id, userID, now())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return nil
	})
}

func finishMaterialization(ctx context.Context, tx *sql.Tx, userID, id string) error {
	if id == "" { // store callers without a filesystem upload
		return nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE uploads SET materialized = 0 WHERE id = ? AND user_id = ? AND expires_at > ?`, id, userID, now())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// ResetMaterialization is called only after a failed attempt's blobs are removed.
func (s *Store) ResetMaterialization(ctx context.Context, userID, id string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE uploads SET materialized = 0 WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

func (s *Store) UploadByID(ctx context.Context, userID, id string) (*Upload, error) {
	var u Upload
	var exp int64
	err := s.DB.QueryRowContext(ctx, `SELECT id, user_id, size, received, materialized, expires_at FROM uploads WHERE id = ? AND user_id = ? AND expires_at > ?`, id, userID, now()).
		Scan(&u.ID, &u.UserID, &u.Size, &u.Received, &u.Materialized, &exp)
	if err != nil {
		return nil, notFound(err)
	}
	u.ExpiresAt = ts(exp)
	return &u, nil
}

func (s *Store) SetUploadReceived(ctx context.Context, id string, received int64, idle time.Duration) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE uploads SET received = ?, expires_at = CASE WHEN ? > received THEN ? ELSE expires_at END WHERE id = ?`, received, received, time.Now().Add(idle).Unix(), id)
	return err
}

func (s *Store) DeleteUpload(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, id)
	return err
}

// ExpiredUpload is an upload past its idle expiry. Materialized is nonzero
// when a crashed or failed rollback may have left unpacked blobs behind.
type ExpiredUpload struct {
	ID           string
	Materialized int64
}

// ExpiredUploads lists uploads whose idle expiry has passed.
func (s *Store) ExpiredUploads(ctx context.Context) ([]ExpiredUpload, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, materialized FROM uploads WHERE expires_at <= ?`, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExpiredUpload
	for rows.Next() {
		var u ExpiredUpload
		if err := rows.Scan(&u.ID, &u.Materialized); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UploadExpired rechecks expiry after the service claims an idle upload, and
// returns its unpacked-byte reservation. A chunk may have renewed it since the
// maintenance candidate list was read.
func (s *Store) UploadExpired(ctx context.Context, id string) (expired bool, materialized int64, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT expires_at <= ?, materialized FROM uploads WHERE id = ?`, now(), id).Scan(&expired, &materialized)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	return expired, materialized, err
}

func (s *Store) UploadExists(ctx context.Context, id string) bool {
	var n int
	s.DB.QueryRowContext(ctx, `SELECT count(*) FROM uploads WHERE id = ?`, id).Scan(&n)
	return n > 0
}

// --- janitor ---

// PurgeArtifacts deletes artifacts whose content retention has ended and
// returns how many were removed.
func (s *Store) PurgeArtifacts(ctx context.Context, expiredRetention, exhaustedWindow time.Duration) (int64, error) {
	n := now()
	res, err := s.DB.ExecContext(ctx, `DELETE FROM artifacts WHERE
		(expires_at IS NOT NULL AND expires_at + ? <= ?) OR
		(exhausted_at IS NOT NULL AND max_views > 0 AND views >= max_views AND exhausted_at + ? <= ?)`,
		int64(expiredRetention.Seconds()), n, int64(exhaustedWindow.Seconds()), n)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) Cleanup(ctx context.Context, viewRetention time.Duration) {
	n := now()
	s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, n)
	s.DB.ExecContext(ctx, `DELETE FROM views WHERE at <= ?`, n-int64(viewRetention.Seconds()))
	s.DB.ExecContext(ctx, `DELETE FROM onetime WHERE used_at IS NULL AND expires_at <= ? AND purpose != 'invite'`, n)
}

// ReferencedBlobs returns the set of blob hashes still referenced.
func (s *Store) ReferencedBlobs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT blob FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out[b] = struct{}{}
	}
	return out, rows.Err()
}

// referenceBatch stays well below SQLite's bound-variable limit.
const referenceBatch = 500

// ReferencedBlobsAmong returns which of hashes some file still references.
// Publication rollback uses it to look up only the blobs it created.
func (s *Store) ReferencedBlobsAmong(ctx context.Context, hashes []string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for start := 0; start < len(hashes); start += referenceBatch {
		batch := hashes[start:min(start+referenceBatch, len(hashes))]
		args := make([]any, len(batch))
		for i, h := range batch {
			args[i] = h
		}
		rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT blob FROM files WHERE blob IN (?`+strings.Repeat(",?", len(batch)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var b string
			if err := rows.Scan(&b); err != nil {
				rows.Close()
				return nil, err
			}
			out[b] = struct{}{}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

type Stats struct {
	Users, Artifacts, Active   int
	LogicalBytes               int64
	StagedBytes, ReservedBytes int64
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	n := now()
	err := s.DB.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM artifacts),
		(SELECT count(*) FROM artifacts WHERE revoked = 0 AND admin_revoked = 0 AND (expires_at IS NULL OR expires_at > ?) AND (max_views = 0 OR views < max_views)),
		(SELECT coalesce(sum(size),0) FROM versions),
		(SELECT coalesce(sum(received),0) FROM uploads),
		(SELECT coalesce(sum(size + materialized),0) FROM uploads)`, n).Scan(&st.Users, &st.Artifacts, &st.Active, &st.LogicalBytes, &st.StagedBytes, &st.ReservedBytes)
	return st, err
}

// slugAttempts bounds retries when a freshly minted slug is already taken.
// With ~2^51.7 slugs a single collision is already improbable.
const slugAttempts = 5

// ErrSlugExhausted means every attempt to mint a unique slug collided.
var ErrSlugExhausted = errors.New("could not mint a unique artifact slug")

// withNewSlug runs exec with fresh slugs until it does not violate the slug
// uniqueness constraint. SQLite rolls back only the failed statement, so the
// surrounding transaction stays usable between attempts.
func (s *Store) withNewSlug(exec func(slug string) error) error {
	for range slugAttempts {
		err := exec(s.newSlug())
		if err == nil || !isSlugConflict(err) {
			return err
		}
	}
	return ErrSlugExhausted
}

func isSlugConflict(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed: artifacts.slug")
}

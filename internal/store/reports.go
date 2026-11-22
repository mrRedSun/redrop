package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
)

// Report sources and statuses (DESIGN.md "Abuse handling").
const (
	ReportVisitor = "visitor" // filed through the report form
	ReportSystem  = "system"  // an upload check's finding

	ReportOpen      = "open"
	ReportResolved  = "resolved"
	ReportDismissed = "dismissed"
)

// Report is one abuse report. It holds no IP address: Network is a keyed
// hash of the reporter's network, kept only while the report is open.
type Report struct {
	ID         string
	Source     string
	ArtifactID string
	OwnerID    string
	Version    int    // system reports: the flagged version
	Kind       string // system reports: the finding kind
	URL        string // visitor reports: as submitted
	Category   string
	Details    string
	Contact    string
	Network    string
	Status     string
	CreatedAt  time.Time
	ClosedAt   *time.Time
	ClosedBy   string

	// Joined: the owner as it is now ("" when the account no longer exists).
	OwnerUsername string
	OwnerDisabled bool
}

// AuditEntry is one moderation action.
type AuditEntry struct {
	At         time.Time
	Actor      string
	Action     string
	ReportID   string
	ArtifactID string
	UserID     string
	Detail     string
}

// Errors of the report actions.
var (
	ErrArtifactGone = errors.New("the reported artifact no longer exists")
	ErrOwnerIsAdmin = errors.New("the owner is an administrator")
)

const reportCols = `r.id, r.source, r.artifact_id, r.owner_id, r.version, r.kind, r.url, r.category, r.details, r.contact, r.network, r.status, r.created_at, r.closed_at, r.closed_by,
	coalesce(u.username, ''), coalesce(u.disabled, 0)`

const reportFrom = ` FROM reports r LEFT JOIN users u ON u.id = r.owner_id`

func scanReport(r scanner) (*Report, error) {
	var rep Report
	var created int64
	var closed sql.NullInt64
	if err := r.Scan(&rep.ID, &rep.Source, &rep.ArtifactID, &rep.OwnerID, &rep.Version, &rep.Kind, &rep.URL, &rep.Category, &rep.Details, &rep.Contact,
		&rep.Network, &rep.Status, &created, &closed, &rep.ClosedBy, &rep.OwnerUsername, &rep.OwnerDisabled); err != nil {
		return nil, notFound(err)
	}
	rep.CreatedAt, rep.ClosedAt = ts(created), tsp(closed)
	return &rep, nil
}

func insertReport(ctx context.Context, tx *sql.Tx, r *Report) error {
	r.ID, r.Status, r.CreatedAt = ids.ID(), ReportOpen, time.Now().UTC().Truncate(time.Second)
	_, err := tx.ExecContext(ctx, `INSERT INTO reports (id, source, artifact_id, owner_id, version, kind, url, category, details, contact, network, status, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, r.Source, r.ArtifactID, r.OwnerID, r.Version, r.Kind, r.URL, r.Category, r.Details, r.Contact, r.Network, r.Status, r.CreatedAt.Unix())
	return err
}

// CountOpenReports counts the open reports from source.
func (s *Store) CountOpenReports(ctx context.Context, source string) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM reports WHERE status = 'open' AND source = ?`, source).Scan(&n)
	return n, err
}

// AddVisitorReport stores r, a visitor report, unless the server already
// has maxOpen open visitor reports, its artifact has maxPerArtifact, or the
// same network (r.Network) already has an open report on it. It reports
// whether r was stored. The checks and the insert share one immediate
// transaction, so concurrent reports cannot pass the caps.
func (s *Store) AddVisitorReport(ctx context.Context, r *Report, maxOpen, maxPerArtifact int) (bool, error) {
	stored := false
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var total, onArtifact, dup int
		if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(artifact_id = ?), 0), coalesce(sum(artifact_id = ? AND network = ?), 0)
			FROM reports WHERE status = 'open' AND source = 'visitor'`, r.ArtifactID, r.ArtifactID, r.Network).Scan(&total, &onArtifact, &dup); err != nil {
			return err
		}
		if total >= maxOpen || onArtifact >= maxPerArtifact || dup > 0 {
			return nil
		}
		r.Source = ReportVisitor
		stored = true
		return insertReport(ctx, tx, r)
	})
	return stored && err == nil, err
}

// AddSystemReport stores r, an upload check's finding, unless an open
// system report already covers it: one per artifact and finding kind, and
// at most one open system report per owner and kind across all the owner's
// artifacts. The second bound keeps one account from filling the queue
// with flags, one artifact at a time. It reports whether r was stored.
func (s *Store) AddSystemReport(ctx context.Context, r *Report) (bool, error) {
	stored := false
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var onArtifact, byOwner int
		if err := tx.QueryRowContext(ctx, `SELECT
				(SELECT count(*) FROM reports WHERE status = 'open' AND source = 'system' AND artifact_id = ? AND kind = ?),
				(SELECT count(*) FROM reports WHERE status = 'open' AND source = 'system' AND owner_id = ? AND kind = ?)`,
			r.ArtifactID, r.Kind, r.OwnerID, r.Kind).Scan(&onArtifact, &byOwner); err != nil {
			return err
		}
		if onArtifact > 0 || byOwner > 0 {
			return nil
		}
		r.Source = ReportSystem
		stored = true
		return insertReport(ctx, tx, r)
	})
	return stored && err == nil, err
}

// ReportByID returns one report.
func (s *Store) ReportByID(ctx context.Context, id string) (*Report, error) {
	return scanReport(s.DB.QueryRowContext(ctx, `SELECT `+reportCols+reportFrom+` WHERE r.id = ?`, id))
}

// ListReports returns one page of the reports with status ("" = every
// status) and source ("" = both sources), newest first, plus the total count
// for the same filter. Visitor reports come before system reports so a
// flood of upload flags cannot bury them: within one source, newest first.
// A page beyond the last one returns no rows, not an error.
func (s *Store) ListReports(ctx context.Context, status, source string, limit, offset int) ([]Report, int, error) {
	where, args := []string{}, []any{}
	if status != "" {
		where, args = append(where, "r.status = ?"), append(args, status)
	}
	if source != "" {
		where, args = append(where, "r.source = ?"), append(args, source)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*)`+reportFrom+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if offset >= total {
		return []Report{}, total, nil
	}
	listArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.DB.QueryContext(ctx, `SELECT `+reportCols+reportFrom+cond+
		` ORDER BY r.source = 'visitor' DESC, r.created_at DESC, r.rowid DESC LIMIT ? OFFSET ?`, listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Report{}
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *r)
	}
	return out, total, rows.Err()
}

// ArtifactsByIDs returns the artifacts among ids that exist, by ID.
func (s *Store) ArtifactsByIDs(ctx context.Context, idList []string) (map[string]*Artifact, error) {
	out := map[string]*Artifact{}
	for len(idList) > 0 {
		chunk := idList[:min(len(idList), 500)]
		idList = idList[len(chunk):]
		rows, err := s.DB.QueryContext(ctx, artifactSelect+` WHERE a.id IN (?`+strings.Repeat(",?", len(chunk)-1)+`)`, anySlice(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			a, err := scanArtifact(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[a.ID] = a
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AuditFor returns the audit entries of the given reports, oldest first.
// Entries with an empty report_id (an undo from the Artifacts or Users
// page) are returned under "" and attached to every listed report that
// names the same artifact or user, so a report's history shows that its
// revocation or suspension was later reversed.
func (s *Store) AuditFor(ctx context.Context, reports []Report) (map[string][]AuditEntry, error) {
	reportIDs := make([]string, 0, len(reports))
	for _, r := range reports {
		reportIDs = append(reportIDs, r.ID)
	}
	out := map[string][]AuditEntry{}
	for len(reportIDs) > 0 {
		chunk := reportIDs[:min(len(reportIDs), 500)]
		reportIDs = reportIDs[len(chunk):]
		rows, err := s.DB.QueryContext(ctx, `SELECT at, actor, action, report_id, artifact_id, user_id, detail FROM audit WHERE report_id IN (?`+
			strings.Repeat(",?", len(chunk)-1)+`) ORDER BY id`, anySlice(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var e AuditEntry
			var at int64
			if err := rows.Scan(&at, &e.Actor, &e.Action, &e.ReportID, &e.ArtifactID, &e.UserID, &e.Detail); err != nil {
				rows.Close()
				return nil, err
			}
			e.At = ts(at)
			out[e.ReportID] = append(out[e.ReportID], e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	// Undo entries with no report_id (an artifact restored, an owner
	// unsuspended from the Artifacts or Users page) belong to the history
	// of every report on that artifact or user.
	return s.auditsForReports(ctx, reports, out)
}

// auditsForReports augments a report's own audit rows with entries whose
// report_id is empty but whose artifact_id or user_id matches. reports is
// the full page the history is being built for; own holds each report's
// own rows, keyed by report ID.
func (s *Store) auditsForReports(ctx context.Context, reports []Report, own map[string][]AuditEntry) (map[string][]AuditEntry, error) {
	artifacts, users := map[string]bool{}, map[string]bool{}
	for _, r := range reports {
		artifacts[r.ArtifactID] = true
		users[r.OwnerID] = true
	}
	aid, uid := make([]string, 0, len(artifacts)), make([]string, 0, len(users))
	for id := range artifacts {
		aid = append(aid, id)
	}
	for id := range users {
		uid = append(uid, id)
	}
	in := func(col string, ids []string) string {
		if len(ids) == 0 {
			return "0"
		}
		return col + " IN (?" + strings.Repeat(",?", len(ids)-1) + ")"
	}
	q := `SELECT at, actor, action, artifact_id, user_id, detail FROM audit WHERE report_id = '' AND ((` + in("artifact_id", aid) + `) OR (` + in("user_id", uid) + `)) ORDER BY id`
	args := append(append([]any{}, anySlice(aid)...), anySlice(uid)...)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var extra []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&at, &e.Actor, &e.Action, &e.ArtifactID, &e.UserID, &e.Detail); err != nil {
			return nil, err
		}
		e.At = ts(at)
		extra = append(extra, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range reports {
		for _, e := range extra {
			if e.ArtifactID == r.ArtifactID || e.UserID == r.OwnerID {
				own[r.ID] = append(own[r.ID], e)
			}
		}
	}
	return own, nil
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, v := range ss {
		out[i] = v
	}
	return out
}

func audit(ctx context.Context, tx *sql.Tx, e AuditEntry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit (at, actor, action, report_id, artifact_id, user_id, detail) VALUES (?,?,?,?,?,?,?)`,
		now(), e.Actor, e.Action, e.ReportID, e.ArtifactID, e.UserID, e.Detail)
	return err
}

// WriteAudit records an action that has no report of its own (an artifact
// restored or an owner unsuspended from the Artifacts or Users page), so
// the reports it undoes can show it in their history.
func (s *Store) WriteAudit(ctx context.Context, e AuditEntry) error {
	return s.Tx(ctx, func(tx *sql.Tx) error { return audit(ctx, tx, e) })
}

// closeOpen resolves the open reports matching where (a condition on
// reports' columns) and returns how many it closed. Closing forgets the
// reporter's network key.
func closeOpen(ctx context.Context, tx *sql.Tx, actor, where string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, `UPDATE reports SET status = 'resolved', closed_at = ?, closed_by = ?, network = '' WHERE status = 'open' AND (`+where+`)`,
		append([]any{now(), actor}, args...)...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetReportStatus sets a report's status and writes its audit entry
// ("report.<status>"). Reopening clears the closing time and administrator.
func (s *Store) SetReportStatus(ctx context.Context, id, status, actor string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var r Report
		if err := tx.QueryRowContext(ctx, `SELECT artifact_id, owner_id FROM reports WHERE id = ?`, id).Scan(&r.ArtifactID, &r.OwnerID); err != nil {
			return notFound(err)
		}
		var err error
		if status == ReportOpen {
			_, err = tx.ExecContext(ctx, `UPDATE reports SET status = 'open', closed_at = NULL, closed_by = '' WHERE id = ?`, id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE reports SET status = ?, closed_at = ?, closed_by = ?, network = '' WHERE id = ?`, status, now(), actor, id)
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, AuditEntry{Actor: actor, Action: "report." + status, ReportID: id, ArtifactID: r.ArtifactID, UserID: r.OwnerID})
	})
}

// RevokeReported gives the reported artifact an administrator's revocation
// (its links stop at once: the access generation changes), resolves every
// open report on it, and writes the audit entry, in one transaction. It
// returns the audit detail.
func (s *Store) RevokeReported(ctx context.Context, reportID, actor string) (string, error) {
	var detail string
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var artifactID, ownerID string
		if err := tx.QueryRowContext(ctx, `SELECT artifact_id, owner_id FROM reports WHERE id = ?`, reportID).Scan(&artifactID, &ownerID); err != nil {
			return notFound(err)
		}
		revoked, err := adminRevoke(ctx, tx, `id = ?`, artifactID)
		if err != nil {
			return err
		}
		// A deleted artifact is gone: refuse rather than resolving reports
		// for something that no longer exists.
		if revoked == 0 {
			return ErrArtifactGone
		}
		closed, err := closeOpen(ctx, tx, actor, `artifact_id = ?`, artifactID)
		if err != nil {
			return err
		}
		detail = fmt.Sprintf("revoked the artifact, resolved %d %s", closed, plural(closed, "report"))
		return audit(ctx, tx, AuditEntry{Actor: actor, Action: "artifact.revoke", ReportID: reportID, ArtifactID: artifactID, UserID: ownerID, Detail: detail})
	})
	return detail, err
}

// SuspendReported suspends the reported artifact's owner, in one
// transaction: the account is disabled and its sessions end, every artifact
// it owns gets an administrator's revocation, every open report on those
// artifacts is resolved, and the audit entry is written. An administrator
// account is refused (ErrOwnerIsAdmin). It returns the audit detail.
func (s *Store) SuspendReported(ctx context.Context, reportID, actor string) (string, error) {
	var detail string
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var artifactID, ownerID, role string
		if err := tx.QueryRowContext(ctx, `SELECT artifact_id, owner_id FROM reports WHERE id = ?`, reportID).Scan(&artifactID, &ownerID); err != nil {
			return notFound(err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, ownerID).Scan(&role); err != nil {
			return notFound(err)
		}
		if role == "admin" {
			return ErrOwnerIsAdmin
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled = 1 WHERE id = ?`, ownerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, ownerID); err != nil {
			return err
		}
		revoked, err := adminRevoke(ctx, tx, `user_id = ?`, ownerID)
		if err != nil {
			return err
		}
		closed, err := closeOpen(ctx, tx, actor, `owner_id = ? OR artifact_id IN (SELECT id FROM artifacts WHERE user_id = ?)`, ownerID, ownerID)
		if err != nil {
			return err
		}
		detail = fmt.Sprintf("suspended the owner, revoked %d %s, resolved %d %s", revoked, plural(revoked, "artifact"), closed, plural(closed, "report"))
		return audit(ctx, tx, AuditEntry{Actor: actor, Action: "user.suspend", ReportID: reportID, ArtifactID: artifactID, UserID: ownerID, Detail: detail})
	})
	return detail, err
}

func plural(n int64, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// adminRevoke gives the artifacts matching where an administrator's
// revocation: admin_revoked is set and the access generation changes, so
// outstanding grants and links stop at once. It returns how many artifacts
// matched. The abuse queue's revoke and suspend paths both go through it,
// so they cannot drift apart.
func adminRevoke(ctx context.Context, tx *sql.Tx, where string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, `UPDATE artifacts SET admin_revoked = 1, access_gen = access_gen + 1, updated_at = ? WHERE `+where,
		append([]any{now()}, args...)...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

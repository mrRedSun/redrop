package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
)

// Mail send states (DESIGN.md "Email").
const (
	MailQueued  = "queued"
	MailSending = "sending"
	MailSent    = "sent"
	MailFailed  = "failed"
	// MaxPendingMail bounds disk usage and the worker's backlog.
	MaxPendingMail = 128
)

var (
	ErrMailFull    = errors.New("mail queue is full")
	ErrMailRate    = errors.New("invite email limit reached")
	ErrMailBusy    = errors.New("invite email is already queued")
	ErrMailBounced = errors.New("address permanently refused mail")
)

// OutgoingMail is one queued message and its delivery state. Times in mail
// tables are nanoseconds, retry schedules included.
//
// An invite row holds no body and no token in any form: ClaimMail rotates
// the invite's token and fills Note, Role and Expires so the worker can
// render the message, with the fresh link, in memory. Alert rows hold their
// rendered digest, which carries no secret.
type OutgoingMail struct {
	ID, To, Kind, InviteID string
	Inviter                string // invite mail: who invited, as rendered at creation
	Body                   []byte
	Status                 string
	Attempts               int
	Error                  string
	CreatedAt, UpdatedAt   time.Time

	// Filled by ClaimMail for invite rows only.
	Note, Role string
	Expires    time.Time
}

func scanMail(r scanner) (*OutgoingMail, error) {
	var m OutgoingMail
	var created, updated int64
	err := r.Scan(&m.ID, &m.To, &m.Kind, &m.InviteID, &m.Inviter, &m.Body, &m.Status, &m.Attempts, &m.Error, &created, &updated)
	if err != nil {
		return nil, notFound(err)
	}
	m.CreatedAt, m.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
	return &m, nil
}

const mailCols = `id,to_addr,kind,coalesce(invite_id,''),inviter,body,status,attempts,error,created_at,updated_at`

func insertMail(ctx context.Context, tx *sql.Tx, m *OutgoingMail, at time.Time) error {
	if m.ID == "" {
		m.ID = ids.ID()
	}
	if m.Body == nil {
		m.Body = []byte{}
	}
	m.CreatedAt, m.UpdatedAt, m.Status = at, at, MailQueued
	var invite any
	if m.InviteID != "" {
		invite = m.InviteID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO mail_out(id,to_addr,kind,invite_id,inviter,body,status,next_at,created_at,updated_at) VALUES(?,?,?,?,?,?,'queued',?,?,?)`,
		m.ID, m.To, m.Kind, invite, m.Inviter, m.Body, at.UnixNano(), at.UnixNano(), at.UnixNano())
	return err
}

func mailAdmission(ctx context.Context, tx *sql.Tx, addr string) error {
	var bounced, pending int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM mail_bounced WHERE address=?), (SELECT count(*) FROM mail_out WHERE status IN ('queued','sending'))`, addr).Scan(&bounced, &pending); err != nil {
		return err
	}
	if bounced > 0 {
		return ErrMailBounced
	}
	if pending >= MaxPendingMail {
		return ErrMailFull
	}
	return nil
}

// chargeInviteMail is transactional, persistent across restarts, and shared
// by the dashboard and offline CLI. Manual resends consume the same budget.
func chargeInviteMail(ctx context.Context, tx *sql.Tx, actor string, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM mail_sends WHERE at<=?`, at.Add(-time.Hour).UnixNano()); err != nil {
		return err
	}
	var total, admin int
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(actor=?),0) FROM mail_sends`, actor).Scan(&total, &admin); err != nil {
		return err
	}
	if total >= 60 || admin >= 10 {
		return ErrMailRate
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO mail_sends(actor,at) VALUES(?,?)`, actor, at.UnixNano())
	return err
}

// CreateMailedInvite stores an emailed invite and its queue row in one
// transaction. hash is the hash of a secret nobody holds: the invite has no
// working link until the worker mints one at send time. A full queue, a
// bounce, or the throttle leaves no orphan invite.
func (s *Store) CreateMailedInvite(ctx context.Context, o *OneTime, hash, actor string, m *OutgoingMail, at time.Time) error {
	if o.ID == "" {
		o.ID = ids.ID()
	}
	o.CreatedAt = at.UTC().Truncate(time.Second)
	m.InviteID, m.Kind, m.Body = o.ID, "invite", nil
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if err := mailAdmission(ctx, tx, m.To); err != nil {
			return err
		}
		if err := chargeInviteMail(ctx, tx, actor, at); err != nil {
			return err
		}
		var by any
		if o.CreatedBy != "" {
			by = o.CreatedBy
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO onetime(id,hash,purpose,created_by,role,quota_bytes,note,email,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, o.ID, hash, PurposeInvite, by, o.Role, o.QuotaBytes, o.Note, o.Email, o.CreatedAt.Unix(), o.ExpiresAt.Unix()); err != nil {
			return err
		}
		return insertMail(ctx, tx, m, at)
	})
}

func (s *Store) MailByInvite(ctx context.Context, id string) (*OutgoingMail, error) {
	return scanMail(s.DB.QueryRowContext(ctx, `SELECT `+mailCols+` FROM mail_out WHERE invite_id=?`, id))
}

// ResendInviteMail queues the invite's email again. The worker mints a new
// token when it sends, so the previous email's link stops working.
func (s *Store) ResendInviteMail(ctx context.Context, id, actor string, at time.Time) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		m, err := scanMail(tx.QueryRowContext(ctx, `SELECT `+mailCols+` FROM mail_out WHERE invite_id=?`, id))
		if err != nil {
			return err
		}
		if m.Status == MailQueued || m.Status == MailSending {
			return ErrMailBusy
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM onetime WHERE id=? AND purpose='invite' AND used_at IS NULL AND expires_at>?`, id, at.Unix()).Scan(&active); err != nil {
			return err
		}
		if active == 0 {
			return ErrLinkUsed
		}
		if err := mailAdmission(ctx, tx, m.To); err != nil {
			return err
		}
		if err := chargeInviteMail(ctx, tx, actor, at); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE mail_out SET status='queued',attempts=0,error='',next_at=?,updated_at=? WHERE id=?`, at.UnixNano(), at.UnixNano(), m.ID)
		return err
	})
}

func (s *Store) MailStatusesForInvites(ctx context.Context, ids []string) (map[string]OutgoingMail, error) {
	out := map[string]OutgoingMail{}
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 500)]
		ids = ids[len(chunk):]
		rows, err := s.DB.QueryContext(ctx, `SELECT `+mailCols+` FROM mail_out WHERE invite_id IN (?`+strings.Repeat(",?", len(chunk)-1)+`)`, anySlice(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			m, err := scanMail(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[m.InviteID] = *m
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RecoverMail returns interrupted attempts to queued. Shutdown calls this
// after its worker stops; startup also calls it after an unclean exit. A
// recovered invite gets a new token when it is claimed again.
func (s *Store) RecoverMail(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE mail_out SET status='queued' WHERE status='sending'`)
	return err
}

// ClaimMail claims the next due row and counts the attempt before any
// network I/O. Alert retries obey the same minimum interval as new digests.
// There is only one serving process.
//
// maxAttempts is the send limit from the queue configuration. A queued row
// that already reached it is failed here, before an invite's hash is rotated:
// an email that did go out on its last attempt (a crash before the outcome
// was saved, then recovery) keeps a working link, and the row says to use
// Send again.
//
// For an invite row, rotate returns the hash of a freshly minted token, and
// the invite's hash is replaced in the same transaction: every attempt
// (first send, retry, Send again, or a retry after a restart) gets a new
// link, and only the latest email's link works. The token itself stays in
// the caller's memory. An invite that is used, expired or gone is cancelled.
func (s *Store) ClaimMail(ctx context.Context, at time.Time, alertInterval time.Duration, maxAttempts int, rotate func() string) (*OutgoingMail, error) {
	var m *OutgoingMail
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		// Cancel mail for links redeemed or expired before sending.
		if _, err = tx.ExecContext(ctx, `UPDATE mail_out SET status='failed',error='invite used or expired' WHERE kind='invite' AND status IN ('queued','sending') AND invite_id IN (SELECT id FROM onetime WHERE used_at IS NOT NULL OR expires_at<=?)`, at.Unix()); err != nil {
			return err
		}
		m, err = scanMail(tx.QueryRowContext(ctx, `SELECT `+mailCols+` FROM mail_out WHERE status='queued' AND next_at<=? AND (kind!='alert' OR to_addr IN (SELECT address FROM mail_alerts WHERE last_attempt<=?)) ORDER BY next_at,created_at LIMIT 1`, at.UnixNano(), at.Add(-alertInterval).UnixNano()))
		if errors.Is(err, ErrNotFound) {
			m = nil
			return nil // commit cancellations even with an empty queue
		}
		if err != nil {
			return err
		}
		// Interrupted at the retry limit: fail the row without rotating the
		// invite's hash, so a link that was already emailed still works.
		if m.Attempts >= maxAttempts {
			_, err = tx.ExecContext(ctx, `UPDATE mail_out SET status='failed',error='interrupted at the retry limit; use Send again',updated_at=? WHERE id=?`, at.UnixNano(), m.ID)
			m = nil
			return err
		}
		var bounced int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM mail_bounced WHERE address=?`, m.To).Scan(&bounced); err != nil {
			return err
		}
		if bounced > 0 {
			_, err = tx.ExecContext(ctx, `UPDATE mail_out SET status='failed',error='address permanently refused mail',body=X'' WHERE id=?`, m.ID)
			m = nil
			return err
		}
		if m.Kind == "invite" {
			var exp int64
			if err = tx.QueryRowContext(ctx, `SELECT note,role,expires_at FROM onetime WHERE id=? AND purpose='invite'`, m.InviteID).Scan(&m.Note, &m.Role, &exp); err != nil {
				return err
			}
			m.Expires = ts(exp)
			res, err := tx.ExecContext(ctx, `UPDATE onetime SET hash=? WHERE id=? AND purpose='invite' AND used_at IS NULL AND expires_at>?`, rotate(), m.InviteID, at.Unix())
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				_, err = tx.ExecContext(ctx, `UPDATE mail_out SET status='failed',error='invite used or expired' WHERE id=?`, m.ID)
				m = nil
				return err
			}
		}
		m.Attempts++
		if _, err = tx.ExecContext(ctx, `UPDATE mail_out SET status='sending',attempts=?,updated_at=? WHERE id=?`, m.Attempts, at.UnixNano(), m.ID); err != nil {
			return err
		}
		if m.Kind == "alert" {
			_, err = tx.ExecContext(ctx, `UPDATE mail_alerts SET last_attempt=? WHERE address=?`, at.UnixNano(), m.To)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrNotFound
	}
	return m, nil
}

// CompleteMail persists an outcome or the next retry time. Only a recipient
// refusal suppresses the address. Finished alerts no longer need their body.
func (s *Store) CompleteMail(ctx context.Context, m *OutgoingMail, status, reason string, next time.Time, bounced bool, at time.Time) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if bounced {
			if _, err := tx.ExecContext(ctx, `INSERT INTO mail_bounced(address,at) VALUES(?,?) ON CONFLICT(address) DO UPDATE SET at=excluded.at`, m.To, at.UnixNano()); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE mail_out SET status=?,error=?,next_at=?,updated_at=?,body=CASE WHEN kind='alert' AND ?!='queued' THEN X'' ELSE body END WHERE id=?`, status, reason, next.UnixNano(), at.UnixNano(), status, m.ID)
		return err
	})
}

// UnbounceMail removes addr from the bounce list, so invites and alerts may
// be sent to it again. It reports whether the address was listed.
func (s *Store) UnbounceMail(ctx context.Context, addr string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM mail_bounced WHERE address=?`, addr)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MailDelay bounds idle polling but wakes at retry/digest deadlines.
func (s *Store) MailDelay(ctx context.Context, at time.Time, interval time.Duration) (time.Duration, error) {
	var next sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT min(due) FROM (
 SELECT CASE WHEN kind='alert' THEN max(next_at,coalesce((SELECT last_attempt+? FROM mail_alerts WHERE address=to_addr),0)) ELSE next_at END AS due FROM mail_out WHERE status='queued'
 UNION ALL SELECT next_at FROM mail_alerts)`, int64(interval)).Scan(&next)
	d := time.Minute
	if next.Valid {
		d = min(d, time.Unix(0, next.Int64).Sub(at))
		if d < time.Millisecond {
			d = time.Millisecond
		}
	}
	return d, err
}

// CleanupMail bounds retention of finished alert rows. Invite rows carry no
// body, so their status stays until the invite is deleted.
func (s *Store) CleanupMail(ctx context.Context, at time.Time) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM mail_out WHERE kind='alert' AND status IN ('sent','failed') AND updated_at<?`, at.Add(-7*24*time.Hour).UnixNano())
	return err
}

// InitMailAlerts starts new recipients at the current report high-water mark,
// rather than emailing historical reports. Existing cursors survive restart.
func (s *Store) InitMailAlerts(ctx context.Context, addresses []string, interval time.Duration, at time.Time) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		for _, addr := range addresses {
			if _, err := tx.ExecContext(ctx, `INSERT INTO mail_alerts(address,cursor,next_at) VALUES(?,coalesce((SELECT max(rowid) FROM reports),0),?) ON CONFLICT(address) DO NOTHING`, addr, at.Add(interval).UnixNano()); err != nil {
				return err
			}
		}
		// Removed recipients must not receive previously queued alerts.
		rows, err := tx.QueryContext(ctx, `SELECT address FROM mail_alerts`)
		if err != nil {
			return err
		}
		var remove []string
		for rows.Next() {
			var addr string
			if err = rows.Scan(&addr); err != nil {
				rows.Close()
				return err
			}
			found := false
			for _, a := range addresses {
				found = found || a == addr
			}
			if !found {
				remove = append(remove, addr)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, addr := range remove {
			if _, err = tx.ExecContext(ctx, `DELETE FROM mail_out WHERE kind='alert' AND to_addr=?`, addr); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM mail_alerts WHERE address=?`, addr); err != nil {
				return err
			}
		}
		return nil
	})
}

// QueueMailAlert snapshots up to 200 new reports, in insertion order. The
// cursor advances atomically with enqueue, so ties in created_at cannot lose
// reports. Remaining reports enter the next digest rather than being dropped.
//
// held is ErrMailBounced or ErrMailFull when a due digest was held back for
// that reason (the deadline moves on; nothing is lost), and nil otherwise.
func (s *Store) QueueMailAlert(ctx context.Context, addr string, interval time.Duration, at time.Time, render func([]Report) (*OutgoingMail, error)) (held error, err error) {
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		held = nil
		var cursor, next, last int64
		if err := tx.QueryRowContext(ctx, `SELECT cursor,next_at,last_attempt FROM mail_alerts WHERE address=?`, addr).Scan(&cursor, &next, &last); err != nil {
			return err
		}
		if at.UnixNano() < next {
			return nil
		}
		deferNext := at.Add(interval).UnixNano()
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mail_out WHERE kind='alert' AND to_addr=? AND status IN ('queued','sending')`, addr).Scan(&pending); err != nil {
			return err
		}
		if pending > 0 {
			_, err := tx.ExecContext(ctx, `UPDATE mail_alerts SET next_at=? WHERE address=?`, deferNext, addr)
			return err
		}
		// The last attempt is stamped when the worker claims the digest,
		// a moment after it was enqueued. Wait for exactly that interval to
		// end, not a whole new one from now, or a steady report stream would
		// get one digest every two intervals.
		if last > 0 && at.Sub(time.Unix(0, last)) < interval {
			_, err := tx.ExecContext(ctx, `UPDATE mail_alerts SET next_at=? WHERE address=?`, time.Unix(0, last).Add(interval).UnixNano(), addr)
			return err
		}
		if err := mailAdmission(ctx, tx, addr); err != nil {
			if errors.Is(err, ErrMailBounced) || errors.Is(err, ErrMailFull) {
				held = err
				_, err = tx.ExecContext(ctx, `UPDATE mail_alerts SET next_at=? WHERE address=?`, deferNext, addr)
			}
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT rowid,id,source,category,created_at FROM reports WHERE rowid>? ORDER BY rowid LIMIT 200`, cursor)
		if err != nil {
			return err
		}
		reports := []Report{}
		for rows.Next() {
			var r Report
			var created int64
			if err = rows.Scan(&cursor, &r.ID, &r.Source, &r.Category, &created); err != nil {
				rows.Close()
				return err
			}
			r.CreatedAt = ts(created)
			reports = append(reports, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(reports) > 0 {
			m, err := render(reports)
			if err != nil {
				return err
			}
			if err = insertMail(ctx, tx, m, at); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE mail_alerts SET cursor=?,next_at=? WHERE address=?`, cursor, deferNext, addr)
		return err
	})
	if err != nil {
		held = nil
	}
	return held, err
}

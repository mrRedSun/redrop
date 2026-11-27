package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
)

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

// OutgoingMail holds delivery metadata and an encrypted RFC 5322 message.
// Times in mail tables use nanoseconds, including retry schedules.
type OutgoingMail struct {
	ID, To, Kind, InviteID string
	Body                   []byte
	Status                 string
	Attempts               int
	Error                  string
	CreatedAt, UpdatedAt   time.Time
}

func scanMail(r scanner) (*OutgoingMail, error) {
	var m OutgoingMail
	var created, updated int64
	err := r.Scan(&m.ID, &m.To, &m.Kind, &m.InviteID, &m.Body, &m.Status, &m.Attempts, &m.Error, &created, &updated)
	if err != nil {
		return nil, notFound(err)
	}
	m.CreatedAt, m.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
	return &m, nil
}

const mailCols = `id,to_addr,kind,coalesce(invite_id,''),body,status,attempts,error,created_at,updated_at`

func insertMail(ctx context.Context, tx *sql.Tx, m *OutgoingMail, at time.Time) error {
	if m.ID == "" {
		m.ID = ids.ID()
	}
	m.CreatedAt, m.UpdatedAt, m.Status = at, at, MailQueued
	var invite any
	if m.InviteID != "" {
		invite = m.InviteID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO mail_out(id,to_addr,kind,invite_id,body,status,next_at,created_at,updated_at) VALUES(?,?,?,?,?,'queued',?,?,?)`, m.ID, m.To, m.Kind, invite, m.Body, at.UnixNano(), at.UnixNano(), at.UnixNano())
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

// CreateMailedInvite stores the hash and encrypted email in one transaction.
// A full queue, bounce, or throttle leaves no orphan invite.
func (s *Store) CreateMailedInvite(ctx context.Context, o *OneTime, hash, actor string, m *OutgoingMail, at time.Time) error {
	if o.ID == "" {
		o.ID = ids.ID()
	}
	o.CreatedAt = at.UTC().Truncate(time.Second)
	m.InviteID = o.ID
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

// ResendInviteMail reuses the original message and never rotates the link.
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
		if active == 0 || len(m.Body) == 0 {
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
		// List views never read ciphertext bodies.
		rows, err := s.DB.QueryContext(ctx, `SELECT id,to_addr,kind,coalesce(invite_id,''),X'',status,attempts,error,created_at,updated_at FROM mail_out WHERE invite_id IN (?`+strings.Repeat(",?", len(chunk)-1)+`)`, anySlice(chunk)...)
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
// after its worker stops; startup also calls it after an unclean exit.
func (s *Store) RecoverMail(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE mail_out SET status='queued' WHERE status='sending'`)
	return err
}

// ClaimMail increments attempts before network I/O. Alert retries obey the
// same minimum interval as new digests. There is only one serving process.
func (s *Store) ClaimMail(ctx context.Context, at time.Time, alertInterval time.Duration) (*OutgoingMail, error) {
	var m *OutgoingMail
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		// Cancel mail for links redeemed, expired, or removed before sending.
		if _, err = tx.ExecContext(ctx, `UPDATE mail_out SET status='failed',error='invite used or expired',body=X'' WHERE kind='invite' AND status IN ('queued','sending') AND invite_id IN (SELECT id FROM onetime WHERE used_at IS NOT NULL OR expires_at<=?)`, at.Unix()); err != nil {
			return err
		}
		m, err = scanMail(tx.QueryRowContext(ctx, `SELECT `+mailCols+` FROM mail_out WHERE status='queued' AND next_at<=? AND (kind!='alert' OR to_addr IN (SELECT address FROM mail_alerts WHERE last_attempt<=?)) ORDER BY next_at,created_at LIMIT 1`, at.UnixNano(), at.Add(-alertInterval).UnixNano()))
		if errors.Is(err, ErrNotFound) {
			return nil
		} // commit cancellations even with an empty queue
		if err != nil {
			return err
		}
		var bounced int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM mail_bounced WHERE address=?`, m.To).Scan(&bounced); err != nil {
			return err
		}
		if bounced > 0 {
			_, err = tx.ExecContext(ctx, `UPDATE mail_out SET status='failed',error='address permanently refused mail',body=CASE WHEN kind='alert' THEN X'' ELSE body END WHERE id=?`, m.ID)
			m = nil
			return err
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
	if err == nil && m == nil {
		return nil, ErrNotFound
	}
	return m, err
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

// CleanupMail clears expired invite secrets but keeps their send status.
// Finished alert rows and old throttle events have bounded retention.
func (s *Store) CleanupMail(ctx context.Context, at time.Time) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE mail_out SET body=X'' WHERE invite_id IN (SELECT id FROM onetime WHERE used_at IS NOT NULL OR expires_at<=?) AND length(body)>0`, at.Unix()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM mail_out WHERE kind='alert' AND status IN ('sent','failed') AND updated_at<?`, at.Add(-7*24*time.Hour).UnixNano())
		return err
	})
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
func (s *Store) QueueMailAlert(ctx context.Context, addr string, interval time.Duration, at time.Time, render func([]Report) (*OutgoingMail, error)) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
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
		if pending > 0 || (last > 0 && at.Sub(time.Unix(0, last)) < interval) {
			_, err := tx.ExecContext(ctx, `UPDATE mail_alerts SET next_at=? WHERE address=?`, deferNext, addr)
			return err
		}
		if err := mailAdmission(ctx, tx, addr); err != nil {
			if errors.Is(err, ErrMailBounced) || errors.Is(err, ErrMailFull) {
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
}

package mail

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/store"
)

// Queue owns one SMTP worker. The database bounds admission to 128 pending
// messages. Retry schedules and attempt counts survive process restarts.
//
// No invite token is ever stored, in clear or sealed: the worker mints the
// token when it claims an invite row (store.ClaimMail), renders the email in
// memory with that link, sends it and drops it. Each attempt therefore has
// its own link, and only the latest email's link works. An SMTP acceptance
// followed by a crash before the outcome is saved sends a second email on
// restart; the first one's link is dead by then.
type Queue struct {
	db         *store.Store
	mailer     Mailer
	log        *slog.Logger
	cfg        QueueConfig
	from       string
	inviteLink func(secret string) string
	wake       chan struct{}
	cancel     context.CancelFunc
	done       chan struct{}
	once       sync.Once
	addresses  []string
	interval   time.Duration
	queueURL   string
	held       map[string]bool // alert recipients whose digest hold was logged
}

// QueueConfig bounds delivery: at most MaxAttempts attempts, with Backoff[i]
// before retry i+1. Alert retries also wait at least the alert interval, and
// an invite is not retried past its expiry.
type QueueConfig struct {
	MaxAttempts int
	Backoff     []time.Duration
}

// DefaultQueueConfig retries over about 36 minutes, so a relay restart or a
// short rate limit does not fail invites for good.
func DefaultQueueConfig() QueueConfig {
	return QueueConfig{MaxAttempts: 4, Backoff: []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}}
}

// NewQueue constructs an idle queue. Start it only in the serving process,
// never in offline administrative tools.
func NewQueue(db *store.Store, m Mailer, from string, log *slog.Logger, cfg QueueConfig) (*Queue, error) {
	if cfg.MaxAttempts < 1 || cfg.MaxAttempts > 4 || len(cfg.Backoff) < cfg.MaxAttempts-1 {
		return nil, errors.New("invalid mail retry schedule")
	}
	return &Queue{db: db, mailer: m, from: from, log: log, cfg: cfg, wake: make(chan struct{}, 1), interval: 10 * time.Minute, held: map[string]bool{}}, nil
}

// SetInviteLink sets how an invite token becomes its absolute link (the
// accounts service owns the path). Call it before Start.
func (q *Queue) SetInviteLink(link func(secret string) string) { q.inviteLink = link }

// Prepare renders a message that holds no secret (a report digest) for
// storage. Invite rows are never prepared: they are rendered at send time.
func (q *Queue) Prepare(m Message, kind string) (*store.OutgoingMail, error) {
	to, err := ValidAddress(m.To)
	if err != nil {
		return nil, err
	}
	m.To = to
	raw, err := RenderWith(q.from, m, time.Now())
	if err != nil {
		return nil, err
	}
	return &store.OutgoingMail{ID: ids.ID(), To: to, Kind: kind, Body: raw}, nil
}

func (q *Queue) Wake() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Start recovers interrupted rows after an unclean stop. Only one process
// may run a worker against the data directory, as for the rest of redrop.
func (q *Queue) Start() error {
	if q.cancel != nil {
		return nil
	}
	if q.inviteLink == nil {
		return errors.New("mail queue started without an invite link builder")
	}
	ctx := context.Background()
	if err := q.db.RecoverMail(ctx); err != nil {
		return err
	}
	if err := q.db.InitMailAlerts(ctx, q.addresses, q.interval, time.Now()); err != nil {
		return err
	}
	ctx, q.cancel = context.WithCancel(ctx)
	q.done = make(chan struct{})
	go q.run(ctx)
	return nil
}

func (q *Queue) Close() {
	q.once.Do(func() {
		if q.cancel != nil {
			q.cancel()
			<-q.done
		}
	})
}

// claim claims the next due row; for an invite, the minted token is returned
// with it and exists nowhere else.
func (q *Queue) claim(ctx context.Context, at time.Time) (*store.OutgoingMail, string, error) {
	var secret string
	m, err := q.db.ClaimMail(ctx, at, q.interval, func() string {
		secret = ids.Secret()
		return ids.Hash(secret)
	})
	return m, secret, err
}

func (q *Queue) run(ctx context.Context) {
	defer close(q.done)
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := q.db.RecoverMail(c); err != nil {
			q.log.Error("mail shutdown state could not be saved")
		}
	}()
	var lastClean time.Time
	for ctx.Err() == nil {
		if time.Since(lastClean) >= time.Hour {
			if err := q.db.CleanupMail(ctx, time.Now()); err != nil {
				q.log.Error("mail cleanup failed")
			}
			lastClean = time.Now()
		}
		alertErr := q.queueAlerts(ctx, time.Now())
		if alertErr != nil && ctx.Err() == nil {
			q.log.Error("report alert enqueue failed")
		}
		m, secret, err := q.claim(ctx, time.Now())
		delay := time.Minute
		switch {
		case err == nil:
			if err = q.deliver(ctx, m, secret); err != nil && ctx.Err() == nil {
				q.log.Error("mail outcome could not be saved")
				delay = time.Second
			} else {
				continue
			}
		case errors.Is(err, store.ErrNotFound):
			if d, e := q.db.MailDelay(ctx, time.Now(), q.interval); e == nil {
				delay = d
			}
		case ctx.Err() != nil:
			return
		default:
			q.log.Error("mail queue lookup failed")
			delay = time.Second
		}
		// A past digest deadline must not turn a database write failure
		// into millisecond polling. Ignore wakeups during error backoff.
		wake := (<-chan struct{})(q.wake)
		if alertErr != nil {
			delay = max(delay, time.Second)
			wake = nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// message returns the bytes to send for a claimed row. An invite is rendered
// now, with the token just minted, a fresh Message-ID and Date.
func (q *Queue) message(m *store.OutgoingMail, secret string) ([]byte, error) {
	if m.Kind != "invite" {
		return m.Body, nil
	}
	msg, err := (InviteEmail{Link: q.inviteLink(secret), Note: m.Note, Inviter: m.Inviter, Expires: m.Expires, Role: m.Role}).Render()
	if err != nil {
		return nil, err
	}
	msg.To = m.To
	return RenderWith(q.from, msg, time.Now())
}

// deliver makes one attempt, leaving any retry scheduled in SQLite rather
// than holding the worker asleep while other messages wait.
func (q *Queue) deliver(ctx context.Context, m *store.OutgoingMail, secret string) error {
	if m.Attempts > q.cfg.MaxAttempts {
		return q.complete(ctx, m, store.MailFailed, "retry limit reached", time.Time{}, false, time.Now())
	}
	raw, err := q.message(m, secret)
	if err != nil {
		return q.complete(ctx, m, store.MailFailed, "email could not be rendered", time.Time{}, false, time.Now())
	}
	sendCtx, cancel := context.WithTimeout(ctx, Timeout)
	err = q.mailer.Send(sendCtx, Message{To: m.To, Raw: raw})
	cancel()
	if ctx.Err() != nil && err != nil {
		return ctx.Err()
	} // an interrupted attempt is queued again by shutdown recovery
	status, reason := store.MailSent, ""
	next := time.Time{}
	bounced := false
	if err != nil {
		status, reason = store.MailFailed, "mail delivery failed"
		// Only our SMTP error type has safe, stage/status-only text. Never
		// persist or log arbitrary implementation errors or remote replies.
		var se *SMTPError
		if errors.As(err, &se) {
			reason = se.Error()
		}
		var permanent *PermanentError
		if errors.As(err, &permanent) {
			bounced = permanent.Recipient
			if se == nil {
				reason = "SMTP configuration or permanent refusal"
			}
		} else if m.Attempts < q.cfg.MaxAttempts {
			wait := q.cfg.Backoff[m.Attempts-1]
			if m.Kind == "alert" {
				wait = max(wait, q.interval)
			}
			next = time.Now().Add(wait)
			if m.Kind == "invite" && !next.Before(m.Expires) {
				reason += "; the invite expires before the next retry"
			} else {
				status = store.MailQueued
			}
		}
	}
	if err := q.complete(ctx, m, status, reason, next, bounced, time.Now()); err != nil {
		return err
	}
	// Neither recipient addresses nor body text belong in operator logs.
	q.log.Info("mail delivery", "mail", m.ID, "kind", m.Kind, "status", status, "attempts", m.Attempts)
	return nil
}

// Retry a failed outcome commit, never the SMTP send that already finished.
// Give an accepted message one bounded commit even during shutdown.
func (q *Queue) complete(ctx context.Context, m *store.OutgoingMail, status, reason string, next time.Time, bounced bool, at time.Time) error {
	logged := false
	for {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := q.db.CompleteMail(writeCtx, m, status, reason, next, bounced, at)
		cancel()
		if err == nil {
			return nil
		}
		if !logged {
			q.log.Error("mail outcome save failed; retrying database write")
			logged = true
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

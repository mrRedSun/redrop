package mail

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/store"
)

// Queue owns one SMTP worker. The database bounds admission to 128 pending
// messages. Retry schedules and attempt counts survive process restarts.
// An SMTP acceptance followed by a crash before commit can deliver twice;
// delivery is at least once, not exactly once.
type Queue struct {
	db        *store.Store
	mailer    Mailer
	log       *slog.Logger
	cfg       QueueConfig
	from      string
	seal      cipher.AEAD
	wake      chan struct{}
	cancel    context.CancelFunc
	done      chan struct{}
	once      sync.Once
	addresses []string
	interval  time.Duration
	queueURL  string
}

type QueueConfig struct {
	MaxAttempts int
	Backoff     []time.Duration
}

func DefaultQueueConfig() QueueConfig {
	return QueueConfig{MaxAttempts: 4, Backoff: []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}}
}

// NewQueue constructs an idle queue. secret is the server signing secret;
// a purpose-derived key encrypts stored message bodies, including invite
// links, so a database snapshot alone does not reveal one-time secrets.
// Start only in the serving process, never in offline administrative tools.
func NewQueue(db *store.Store, m Mailer, from string, secret []byte, log *slog.Logger, cfg QueueConfig) (*Queue, error) {
	if len(secret) < 32 {
		return nil, errors.New("mail queue needs the server signing secret")
	}
	key := sha256.Sum256(append([]byte("redrop-mail-body-v1\x00"), secret...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	seal, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if cfg.MaxAttempts < 1 || cfg.MaxAttempts > 4 || len(cfg.Backoff) < cfg.MaxAttempts-1 {
		return nil, errors.New("invalid mail retry schedule")
	}
	return &Queue{db: db, mailer: m, from: from, log: log, cfg: cfg, seal: seal, wake: make(chan struct{}, 1), interval: 10 * time.Minute}, nil
}

// Prepare renders and encrypts a message without I/O. The caller commits
// it together with its invite, then calls Wake. The ID binds the encrypted
// bytes to their recipient and kind, preventing row swaps.
func (q *Queue) Prepare(m Message, kind, inviteID string) (*store.OutgoingMail, error) {
	to, err := ValidAddress(m.To)
	if err != nil {
		return nil, err
	}
	m.To = to
	raw, err := RenderWith(q.from, m, time.Now())
	if err != nil {
		return nil, err
	}
	row := &store.OutgoingMail{ID: ids.ID(), To: to, Kind: kind, InviteID: inviteID}
	row.Body = q.encrypt(raw, row)
	return row, nil
}
func mailAAD(m *store.OutgoingMail) []byte { return []byte(m.ID + "\x00" + m.To + "\x00" + m.Kind) }
func (q *Queue) encrypt(raw []byte, m *store.OutgoingMail) []byte {
	nonce := make([]byte, q.seal.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return q.seal.Seal(nonce, nonce, raw, mailAAD(m))
}
func (q *Queue) decrypt(m *store.OutgoingMail) ([]byte, error) {
	n := q.seal.NonceSize()
	if len(m.Body) < n {
		return nil, errors.New("invalid encrypted mail")
	}
	return q.seal.Open(nil, m.Body[:n], m.Body[n:], mailAAD(m))
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
		m, err := q.db.ClaimMail(ctx, time.Now(), q.interval)
		delay := time.Minute
		switch {
		case err == nil && m != nil:
			if err = q.deliver(ctx, m); err != nil && ctx.Err() == nil {
				q.log.Error("mail outcome could not be saved")
				delay = time.Second
			} else {
				continue
			}
		case err == nil:
			continue
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

// deliver makes one attempt, leaving any retry scheduled in SQLite rather
// than holding the worker asleep while other messages wait.
func (q *Queue) deliver(ctx context.Context, m *store.OutgoingMail) error {
	if m.Attempts > q.cfg.MaxAttempts {
		return q.complete(ctx, m, store.MailFailed, "retry limit reached", time.Time{}, false, time.Now())
	}
	raw, err := q.decrypt(m)
	if err != nil {
		return q.complete(ctx, m, store.MailFailed, "stored email could not be decrypted", time.Time{}, false, time.Now())
	}
	sendCtx, cancel := context.WithTimeout(ctx, Timeout)
	err = q.mailer.Send(sendCtx, Message{To: m.To, Raw: raw})
	cancel()
	if ctx.Err() != nil && err != nil {
		return ctx.Err()
	} // an interrupted attempt remains queued after shutdown recovery
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
			status = store.MailQueued
			wait := q.cfg.Backoff[m.Attempts-1]
			if m.Kind == "alert" {
				wait = max(wait, q.interval)
			}
			next = time.Now().Add(wait)
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

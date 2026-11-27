package mail

import (
	"context"
	"time"

	"github.com/mrRedSun/redrop/internal/store"
)

// SetAlerts configures report digests before Start. A persisted insertion
// cursor handles reports with identical timestamps and survives restarts.
func (q *Queue) SetAlerts(addresses []string, interval time.Duration, queueURL string) {
	q.addresses = append([]string(nil), addresses...)
	if interval > 0 {
		q.interval = interval
	}
	q.queueURL = queueURL
}

// Notify is the existing sharing report hook's email adapter. It only
// wakes the worker; no SMTP or database work runs in the request.
func (q *Queue) Notify(reportID string) { q.Wake() }

func (q *Queue) queueAlerts(ctx context.Context, at time.Time) error {
	for _, addr := range q.addresses {
		err := q.db.QueueMailAlert(ctx, addr, q.interval, at, func(reports []store.Report) (*store.OutgoingMail, error) {
			list := make([]ReportAlert, 0, len(reports))
			for _, r := range reports {
				list = append(list, ReportAlert{ID: r.ID, Category: r.Category, Source: r.Source, Created: r.CreatedAt})
			}
			m, err := (AlertEmail{QueueURL: q.queueURL, Reports: list, Interval: q.interval}).Render()
			if err != nil {
				return nil, err
			}
			m.To = addr
			return q.Prepare(m, "alert", "")
		})
		if err != nil {
			return err
		}
	}
	return nil
}

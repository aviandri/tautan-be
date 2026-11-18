package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/aviandri/tautan-be/internal/mail"
)

// Email goes through the email_outbox table: handlers enqueue (inside their
// transaction when they have one) and RunMailer delivers with retries. Other
// features (order receipts, download links) should use enqueueEmail too.

const (
	outboxBatch       = 20
	outboxLease       = 2 * time.Minute // a claimed row isn't retried by another worker before this
	outboxMaxAttempts = 8
	outboxSendTimeout = 30 * time.Second
)

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// enqueueEmail stores m for delivery. Pass a pgx.Tx to send only if the
// surrounding transaction commits.
func (s *Server) enqueueEmail(ctx context.Context, q execer, kind string, m mail.Message) error {
	_, err := q.Exec(ctx, `INSERT INTO email_outbox (kind, to_email, subject, text_body, html_body, reply_to, next_attempt_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`, kind, m.To, m.Subject, m.Text, m.HTML, m.ReplyTo, s.now())
	if err == nil {
		s.wakeMailer()
	}
	return err
}

func (s *Server) wakeMailer() {
	select {
	case s.mailWake <- struct{}{}:
	default:
	}
}

// outboxBackoff is the wait after the n-th failed attempt: 30 s doubling, capped at 1 h.
func outboxBackoff(n int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < n && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// DeliverPending sends one batch of due messages and reports how many it claimed.
func (s *Server) DeliverPending(ctx context.Context) (int, error) {
	now := s.now()
	rows, err := s.db.Query(ctx, `UPDATE email_outbox SET attempts = attempts + 1, next_attempt_at = $2
		WHERE id IN (SELECT id FROM email_outbox WHERE status = 'pending' AND next_attempt_at <= $1
		             ORDER BY id LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING id, kind, to_email, subject, text_body, html_body, reply_to, attempts`, now, now.Add(outboxLease), outboxBatch)
	if err != nil {
		return 0, err
	}
	type job struct {
		id       int64
		kind     string
		m        mail.Message
		attempts int
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.kind, &j.m.To, &j.m.Subject, &j.m.Text, &j.m.HTML, &j.m.ReplyTo, &j.attempts); err != nil {
			rows.Close()
			return 0, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, j := range jobs {
		sctx, cancel := context.WithTimeout(ctx, outboxSendTimeout)
		err := s.mailer.Send(sctx, j.m)
		cancel()
		if err == nil {
			_, err = s.db.Exec(ctx, `UPDATE email_outbox SET status = 'sent', sent_at = $2, text_body = '', html_body = '', last_error = NULL WHERE id = $1`, j.id, s.now())
			if err != nil {
				slog.Error("outbox: mark sent", "id", j.id, "err", err)
			}
			continue
		}
		status, next := "pending", s.now().Add(outboxBackoff(j.attempts))
		if j.attempts >= outboxMaxAttempts {
			status = "failed"
		}
		slog.Warn("outbox: send failed", "id", j.id, "kind", j.kind, "attempt", j.attempts, "final", status == "failed", "err", err)
		if _, err := s.db.Exec(ctx, `UPDATE email_outbox SET status = $2, next_attempt_at = $3, last_error = $4,
			text_body = CASE WHEN $2 = 'failed' THEN '' ELSE text_body END,
			html_body = CASE WHEN $2 = 'failed' THEN '' ELSE html_body END WHERE id = $1`,
			j.id, status, next, truncate(err.Error(), 500)); err != nil {
			slog.Error("outbox: mark failed", "id", j.id, "err", err)
		}
	}
	return len(jobs), nil
}

// RunMailer delivers the outbox until ctx is done: on every enqueue, and every
// few seconds for retries and messages left by a previous process.
func (s *Server) RunMailer(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		for {
			n, err := s.DeliverPending(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("outbox: deliver", "err", err)
				}
				break
			}
			if n < outboxBatch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.mailWake:
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// QueuedNotification is the payload the schema's trigger publishes when a job
// becomes claimable.
type QueuedNotification struct {
	JobID string   `json:"job_id"`
	Tags  []string `json:"tags"`
	// RunnerID is populated for cancel notifications; the job-queued trigger
	// leaves it empty.
	RunnerID string `json:"runner_id,omitempty"`
}

// Listen blocks, delivering a signal on every job-queued notification until the
// context is cancelled.
//
// The doorbell is an optimization, never a correctness requirement. Notifications
// are fire-and-forget: a dispatcher that is reconnecting when one is published
// simply misses it. That is why callers pair Listen with a periodic tick — the
// notification makes the common case fast, the tick makes every case correct.
//
// This takes its own dedicated connection rather than a pooled one. A connection
// in LISTEN state is not reusable for queries, so borrowing one from the pool
// for the process's lifetime would quietly shrink the pool.
func Listen(ctx context.Context, dsn string, onQueued func(QueuedNotification)) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if err := listenOnce(ctx, dsn, NotifyChannel, onQueued); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A dropped LISTEN connection loses notifications until it is back.
			// The caller's periodic sweep covers the gap, so reconnecting with
			// backoff is enough; there is nothing to replay.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
	}
}

// ListenCancels is to a cancel request what Listen is to a new job: it rings
// the dispatcher's cancelWake channel when a cancel request lands, so a job that
// is cancelled gets its CancelJob pushed within the polling window instead of
// waiting for the next tick. The two channels are independent and run on their
// own connections, so a drop in one never blocks the other.
func ListenCancels(ctx context.Context, dsn string, onQueued func(QueuedNotification)) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if err := listenOnce(ctx, dsn, CancelChannel, onQueued); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
	}
}

// listenOnce is the one-connection listen loop shared by Listen and
// ListenCancels. Each caller passes the channel name it wants to listen on.
func listenOnce(ctx context.Context, dsn, channel string, onQueued func(QueuedNotification)) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("queue: listen connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return fmt.Errorf("queue: LISTEN %s: %w", channel, err)
	}

	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("queue: wait for notification: %w", err)
		}
		var payload QueuedNotification
		if err := json.Unmarshal([]byte(notification.Payload), &payload); err != nil {
			// A malformed payload is not worth dropping the connection over —
			// keep listening on the same connection rather than reconnecting —
			// but it should never happen (the trigger always emits valid JSON),
			// so it's worth knowing about if it ever does.
			slog.Warn("queue: malformed notification payload",
				"channel", channel, "payload", notification.Payload, "error", err)
			continue
		}
		onQueued(payload)
	}
}

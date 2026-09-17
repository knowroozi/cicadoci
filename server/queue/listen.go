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
		if err := listenOnce(ctx, dsn, onQueued); err != nil {
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

func listenOnce(ctx context.Context, dsn string, onQueued func(QueuedNotification)) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("queue: listen connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		return fmt.Errorf("queue: LISTEN: %w", err)
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
			slog.Warn("queue: malformed job-queued notification payload",
				"payload", notification.Payload, "error", err)
			continue
		}
		onQueued(payload)
	}
}

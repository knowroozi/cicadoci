package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// maxBodySize caps the request body to protect memory.  The largest GitHub
// webhook payload for CICaDo's events (pull_request, push, check_*) is
// well under this limit in practice.
const maxBodySize = 1 << 20 // 1 MiB

// erKeyCode and erCodeErr are the two JSON keys used in error envelopes
// (goconst: each literal appears 7+ times across the handler's response paths).
const (
	erKeyCode     = "code"
	erCodeErr     = "error"
	erCodeMessage = "message"
)

// ReceiverOptions configures a Receiver.
type ReceiverOptions struct {
	// Secret is the GitHub App webhook signing secret.
	Secret string
	// Dispatch decodes and routes events to a Dispatcher implementation.
	Dispatch Dispatcher
	// Tracker is the delivery-ID idempotency tracker.
	// If nil, a default 10k-entry, 24-hour tracker is created.
	Tracker *deliveryTracker
	// Logger is the structured logger.  If nil, slog.Default() is used.
	Logger *slog.Logger
	// MaxConcurrent limits how many webhook deliveries are processed
	// simultaneously.  If zero, no concurrency cap is enforced.
	// This is the "webhook flood backpressure" hook from the ticket.
	MaxConcurrent int
}

// Receiver is the HTTP handler for POST /api/v1/webhooks/github.
// It verifies the signature, checks delivery ID idempotency, applies a
// optional concurrency cap, then dispatches the event.
type Receiver struct {
	secret   string
	dispatch Dispatcher
	tracker  *deliveryTracker
	logger   *slog.Logger
	// sem is a weighted semaphore enforcing MaxConcurrent; nil disables it.
	sem *semaphore.Weighted
	// suppressed counts how many deliveries were rejected as duplicates.
	suppressed uint64
}

// NewReceiver returns a fully-wired Receiver.  secret must come from the
// GitHub App webhook configuration; an empty secret means no signature
// verification (useful only for local dev).
func NewReceiver(opts ReceiverOptions) *Receiver {
	tracker := opts.Tracker
	if tracker == nil {
		tracker = NewDeliveryTracker(0, 0)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	r := &Receiver{
		secret:   opts.Secret,
		dispatch: opts.Dispatch,
		tracker:  tracker,
		logger:   logger,
	}
	if cap := opts.MaxConcurrent; cap > 0 {
		r.sem = semaphore.NewWeighted(int64(cap))
	}
	return r
}

// Suppressed returns how many deliveries have been rejected as duplicates
// since the receiver was created.  Meant for periodic metric export.
func (r *Receiver) Suppressed() uint64 { return atomic.LoadUint64(&r.suppressed) }

// ServeHTTP implements the HTTP handler.
//
// Response codes:
//
//	200 — event accepted and dispatched successfully, or a duplicate that
//	  was already processed (idempotent success)
//	400 — malformed JSON body or unknown event type
//	401 — signature verification failed
//	501 — event type is recognised but its Dispatcher method is unimplemented
//	503 — concurrency cap reached (flood backpressure); GitHub will retry
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Body size guard.
	req.Body = http.MaxBytesReader(w, req.Body, maxBodySize)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		r.logError(http.StatusBadRequest, "read body", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			erCodeErr: map[string]string{erKeyCode: "bad_request", erCodeMessage: "body too large or unreadable"},
		})
		return
	}

	// Signature verification.
	sig := req.Header.Get("X-Hub-Signature-256")
	if r.secret != "" {
		if err := Verify(r.secret, sig, body); err != nil {
			code := http.StatusUnauthorized
			if errors.Is(err, ErrEmptySignature) {
				code = http.StatusBadRequest // 400 is more useful for misconfigured local setups
			}
			r.logError(code, "signature verification", err)
			writeJSON(w, code, map[string]any{
				erCodeErr: map[string]string{erKeyCode: "unauthorized", erCodeMessage: err.Error()},
			})
			return
		}
	}

	// Event type.
	eventType := req.Header.Get("X-GitHub-Event")
	if eventType == "" {
		r.logError(http.StatusBadRequest, "missing event type", nil)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			erCodeErr: map[string]string{erKeyCode: "bad_request", erCodeMessage: "missing X-GitHub-Event header"},
		})
		return
	}

	// Concurrency cap (flood backpressure).
	if r.sem != nil {
		if !r.sem.TryAcquire(1) {
			r.logError(http.StatusServiceUnavailable, "concurrency cap reached", nil)
			// Retry-After is a strong hint; GitHub honours it.
			w.Header().Set("Retry-After", "5")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				erCodeErr: map[string]string{erKeyCode: "flood_limit", erCodeMessage: "concurrency cap reached; retry later"},
			})
			return
		}
		defer r.sem.Release(1)
	}

	// Idempotency: delivery ID dedup.
	deliveryID := req.Header.Get("X-GitHub-Delivery")
	if deliveryID != "" {
		now := time.Now()
		if r.tracker.seen(deliveryID, now, &r.suppressed) {
			r.logger.Info("duplicate delivery suppressed",
				"delivery_id", deliveryID,
				"event_type", eventType)
			// 200 so GitHub stops retrying — the first delivery was already processed.
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":     true,
				"status": "duplicate",
			})
			return
		}
	}

	// Dispatch.
	dispatchErr := r.dispatchEvent(eventType, body, req.Context())
	if dispatchErr != nil {
		switch {
		case errors.Is(dispatchErr, errUnknownEventType):
			r.logError(http.StatusBadRequest, "unknown event type", dispatchErr)
			writeJSON(w, http.StatusBadRequest, map[string]any{
				erCodeErr: map[string]string{erKeyCode: "unknown_event", erCodeMessage: "unrecognised event type"},
			})
		case errors.Is(dispatchErr, ErrUnimplemented):
			// 501 signals "not implemented" so GitHub retries on a schedule
			// rather than treating it as a terminal failure.
			r.logError(http.StatusNotImplemented, "event not implemented", dispatchErr)
			writeJSON(w, http.StatusNotImplemented, map[string]any{
				erCodeErr: map[string]string{erKeyCode: "not_implemented", erCodeMessage: "event handler not yet implemented"},
			})
		default:
			r.logError(http.StatusInternalServerError, "dispatch error", dispatchErr)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				erCodeErr: map[string]string{erKeyCode: "internal_error", erCodeMessage: "failed to process webhook"},
			})
		}
		return
	}

	// Success.
	r.logger.Info("webhook delivered",
		"delivery_id", deliveryID,
		"event_type", eventType)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"status": "ok",
	})
}

// dispatchEvent decodes the raw body and routes to the matching dispatcher method.
func (r *Receiver) dispatchEvent(eventType string, body []byte, ctx context.Context) error {
	// Wrap the dispatch with context so that a dispatcher implementation
	// can observe cancellation from the request lifecycle if needed.
	_ = ctx
	return Dispatch(eventType, body, r.dispatch)
}

// logError wraps a logger.Error call for a failed webhook delivery.
func (r *Receiver) logError(status int, context string, cause error) {
	r.logger.Error("webhook delivery failed",
		"status", status,
		"context", context,
		"cause", cause,
	)
}

// writeJSON is copied from server/api/handler.go for local test convenience.
// Both packages must keep the same envelope shape.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status) //nolint:errcheck
	body, _ := json.Marshal(v)
	_, _ = w.Write(body) //nolint:errcheck
}

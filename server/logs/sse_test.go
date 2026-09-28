package logs

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/archive"
)

// TestSSE_WritesEvents verifies that the SSE handler writes a
// well-formed text/event-stream for a published entry.
func TestSSE_WritesEvents(t *testing.T) {
	h := NewHub(8)
	sse := NewSSE(h, nil) // no compactor — live-only

	ctx, cancel := context.WithCancel(t.Context())
	jobID := uuid.New()

	// Background goroutine publishes an entry shortly after the
	// subscription is set up.
	go func() {
		time.Sleep(20 * time.Millisecond)
		h.Publish(jobID, archive.Entry{Seq: 1, Data: []byte("hello\n")})
		time.Sleep(20 * time.Millisecond)
		h.Publish(jobID, archive.Entry{Seq: 2, Data: []byte("world\n")})
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	r := httptest.NewRequestWithContext(ctx, "GET",
		"/api/v1/logs?job="+jobID.String(), nil)
	w := httptest.NewRecorder()
	sse.ServeHTTP(w, r)

	body := w.Body.String()
	// Verify both events appear in order.
	idx1 := strings.Index(body, "id: 1\n")
	idx2 := strings.Index(body, "id: 2\n")
	if idx1 == -1 || idx2 == -1 {
		t.Fatalf("missing events:\n%s", body)
	}
	if idx1 > idx2 {
		t.Fatal("event 1 appears after event 2")
	}
	if !strings.Contains(body, "event: log\n") {
		t.Fatalf("missing 'event: log' fields:\n%s", body)
	}
	if !strings.Contains(body, "data: hello") {
		t.Fatalf("missing hello data:\n%s", body)
	}
	_ = io.EOF
}

// TestSSE_BadJobID returns 400 when the job query param is not a UUID.
func TestSSE_BadJobID(t *testing.T) {
	h := NewHub(8)
	sse := NewSSE(h, nil)

	r := httptest.NewRequestWithContext(t.Context(), "GET",
		"/api/v1/logs?job=not-a-uuid", nil)
	w := httptest.NewRecorder()
	sse.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

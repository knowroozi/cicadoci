package runnersvc

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These tests cover the Throttler in isolation, without a DB or stream, because
// the throttling decision is the core of CIC-30 and it has no DB dependency.
// The LogStream handler wiring is covered by the M4 E2E test once the full stack
// is in place.

func TestThrottler_DisabledByZeroLimit(t *testing.T) {
	t.Parallel()
	tt := NewThrottler(0)
	job := uuid.New()
	for i := 0; i < 100; i++ {
		if tt.Account(job, 1_000_000) {
			t.Fatalf("throttler with limit=0 must never pause; pause #%d", i)
		}
	}
	if tt.ShouldThrottle(job) {
		t.Fatal("ShouldThrottle must be false when limit=0")
	}
	// Ack on a disabled throttler is a no-op and must not panic.
	tt.Ack(job, 1_000_000)
}

func TestThrottler_PausesAtLimit(t *testing.T) {
	t.Parallel()
	const limit = 1000
	tt := NewThrottler(limit)
	job := uuid.New()

	// 5 chunks of 200 bytes: 1000 total, exactly at the limit, no pause.
	for i := 0; i < 5; i++ {
		if tt.Account(job, 200) {
			t.Fatalf("chunk %d should not pause at limit boundary", i)
		}
	}
	if tt.ShouldThrottle(job) {
		t.Fatal("ShouldThrottle must be false at exactly the limit")
	}

	// One more byte over the limit triggers a stick pause.
	if !tt.Account(job, 1) {
		t.Fatal("1 over limit must return true")
	}
	if !tt.ShouldThrottle(job) {
		t.Fatal("ShouldThrottle must be true after exceeding limit")
	}

	// Once paused, more bytes stay paused and still report true.
	if !tt.Account(job, 1_000_000) {
		t.Fatal("already-paused job must stay true")
	}
}

func TestThrottler_AckReleasesPause(t *testing.T) {
	t.Parallel()
	tt := NewThrottler(1000)
	job := uuid.New()
	tt.Account(job, 1000)
	tt.Account(job, 1) // pause

	if !tt.ShouldThrottle(job) {
		t.Fatal("precondition: ShouldThrottle must be true after pause")
	}
	// Ack for the full 1001 bytes clears the pause and drops the entry.
	tt.Ack(job, 1001)
	if tt.ShouldThrottle(job) {
		t.Fatal("Ack for the full inflight count must release pause")
	}
	// After release, a fresh 900-byte chunk fits under the limit.
	if tt.Account(job, 900) {
		t.Fatal("after a full Ack, a 900-byte chunk must not pause")
	}
}

func TestThrottler_PartialAck(t *testing.T) {
	t.Parallel()
	tt := NewThrottler(1000)
	job := uuid.New()
	tt.Account(job, 600)
	if tt.ShouldThrottle(job) {
		t.Fatal("600 of 1000 must not pause")
	}

	// Ack just half the inflight: 600 - 400 = 200 remaining.
	tt.Ack(job, 400)
	// 200 + 800 = 1000, fits exactly.
	if tt.Account(job, 800) {
		t.Fatal("after a 400-byte partial Ack, 800 more must fit")
	}
	// 1 more over.
	if !tt.Account(job, 1) {
		t.Fatal("one byte past the boundary must pause")
	}
}

func TestThrottler_ConcurrencySafe(t *testing.T) {
	t.Parallel()
	tt := NewThrottler(1_000_000)

	var (
		wg   sync.WaitGroup
		jobs = make([]uuid.UUID, 16)
	)
	for i := range jobs {
		jobs[i] = uuid.New()
	}

	for i := 0; i < 64; i++ {
		wg.Add(1)
		job := jobs[i%len(jobs)]
		go func(job uuid.UUID) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				if tt.Account(job, 64) {
					// A pause may or may not happen depending on other
					// goroutines; just exercise the path.
					_ = tt.ShouldThrottle(job)
				}
				tt.Ack(job, 64)
			}
		}(job)
	}
	wg.Wait()
}

func TestThrottler_AckUnknownJobIsNoOp(t *testing.T) {
	t.Parallel()
	tt := NewThrottler(1000)
	// Ack for a job that has never been Accounted must not panic or report
	// throttling.
	tt.Ack(uuid.New(), 10_000)
	if tt.ShouldThrottle(uuid.New()) {
		t.Fatal("ShouldThrottle must be false for an unaccounted job")
	}
}

func TestLogStreamConfig_Defaults(t *testing.T) {
	t.Parallel()
	cfg := DefaultLogStreamConfig()
	if cfg.MaxLogChunkBytes != 64*1024 {
		t.Fatalf("MaxLogChunkBytes = %d, want 65536", cfg.MaxLogChunkBytes)
	}
	if cfg.MaxInflightChunks != 1024 {
		t.Fatalf("MaxInflightChunks = %d, want 1024", cfg.MaxInflightChunks)
	}
	if cfg.AckInterval != 200*time.Millisecond {
		t.Fatalf("AckInterval = %v, want 200ms", cfg.AckInterval)
	}
}

func TestLogBatch_AddIdempotentForSeq(t *testing.T) {
	t.Parallel()
	b := newLogBatch()
	job := uuid.New()
	b.add(job, 10, 100)
	b.add(job, 5, 50) // lower seq must not clobber the higher watermark
	if got := b.seqs[job]; got != 10 {
		t.Fatalf("seq = %d, want 10 (highest wins)", got)
	}
	// Bytes accumulate, unlike seqs.
	if got := b.bytes[job]; got != 150 {
		t.Fatalf("bytes = %d, want 150 (sum of additions)", got)
	}
}

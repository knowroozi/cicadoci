package runnersvc

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
)

// Default log ingestion timings. The proto default for max_log_chunk_bytes is
// 65536 (64 KiB); the ack watermark tick is 200 ms (CIC-30).
const (
	// DefaultMaxLogChunkBytes is the upper bound on the size of a single
	// LogChunk.data; chunks that declare more bytes than this are rejected.
	DefaultMaxLogChunkBytes uint32 = 64 * 1024
	// DefaultMaxInflightChunks caps the number of un-acked chunks per job
	// before the server asks the runner to pause via Throttle.
	DefaultMaxInflightChunks uint32 = 1024
	// defaultAckInterval is how often the server advances the watermark and
	// emits a LogAck. Ticking too often doubles the busiest message rate for
	// no benefit; 200 ms matches the runner-side batching window (CIC-30).
	defaultAckInterval = 200 * time.Millisecond
)

// LogStreamConfig configures the LogStream handler.
type LogStreamConfig struct {
	MaxLogChunkBytes  uint32
	MaxInflightChunks uint32
	// AckInterval is how often the watermark is advanced. When zero, the
	// handler uses defaultAckInterval.
	AckInterval time.Duration
}

// DefaultLogStreamConfig returns the defaults from the proto Limits message.
func DefaultLogStreamConfig() LogStreamConfig {
	return LogStreamConfig{
		MaxLogChunkBytes:  DefaultMaxLogChunkBytes,
		MaxInflightChunks: DefaultMaxInflightChunks,
		AckInterval:       defaultAckInterval,
	}
}

// Throttler tracks per-job inflight bytes and decides when to ask a runner to
// slow down. It is safe for concurrent use from the LogStream goroutine and
// the Throttle-emit loop on the session stream.
type Throttler struct {
	limitPerJobBytes uint64

	mu       sync.Mutex
	inFlight map[uuid.UUID]int64 // unacked bytes; -1 == paused (sticky)
}

// NewThrottler returns a Throttler that throttles when a job's unacked byte
// count exceeds limitPerJobBytes. A limit of 0 disables throttling.
func NewThrottler(limitPerJobBytes uint64) *Throttler {
	return &Throttler{
		limitPerJobBytes: limitPerJobBytes,
		inFlight:         make(map[uuid.UUID]int64),
	}
}

// Account registers that jobID has newly-sent unacked bytes. A pause (limit
// reached) is sticky until Ack is called for that job; the runner is told to
// stop sending via the Throttle control message.
func (t *Throttler) Account(jobID uuid.UUID, bytes uint64) bool {
	if t.limitPerJobBytes == 0 {
		return false
	}
	const pause = int64(-1)
	t.mu.Lock()
	cur := t.inFlight[jobID]
	if cur == 0 {
		cur = 0 // treat missing as zero below
	}
	if cur == pause {
		t.mu.Unlock()
		return true // already paused
	}
	cur += int64(bytes)
	over := uint64(cur) > t.limitPerJobBytes
	if over {
		t.inFlight[jobID] = pause
	} else {
		t.inFlight[jobID] = cur
	}
	t.mu.Unlock()
	return over
}

// Ack records that bytes for jobID have been persisted; it drops the job's
// inflight count and clears any pause flag.
func (t *Throttler) Ack(jobID uuid.UUID, bytes uint64) {
	if t.limitPerJobBytes == 0 {
		return
	}
	t.mu.Lock()
	if cur, ok := t.inFlight[jobID]; ok {
		if cur == -1 {
			// A pause had been sticky; a single Ack fully releases it.
			delete(t.inFlight, jobID)
		} else if cur <= int64(bytes) {
			delete(t.inFlight, jobID)
		} else {
			t.inFlight[jobID] = cur - int64(bytes)
		}
	}
	t.mu.Unlock()
}

// ShouldThrottle returns true when a job has crossed its limit.
func (t *Throttler) ShouldThrottle(jobID uuid.UUID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inFlight[jobID] == -1
}

// logBatch is a single flushed group of per-job high-water marks.
type logBatch struct {
	seqs  map[uuid.UUID]uint64
	bytes map[uuid.UUID]uint64
}

func newLogBatch() *logBatch {
	return &logBatch{
		seqs:  make(map[uuid.UUID]uint64),
		bytes: make(map[uuid.UUID]uint64),
	}
}

func (b *logBatch) add(jobID uuid.UUID, seq uint64, nBytes uint64) {
	if seq > b.seqs[jobID] {
		b.seqs[jobID] = seq
	}
	b.bytes[jobID] += nBytes
}

// LogStream is the bulk lane: log chunks and structured events up, watermark
// acks down.
//
// It is a separate stream from Session on the same connection so that a job
// producing a flood of output cannot delay a cancellation or a heartbeat behind
// it in one HTTP/2 stream's flow-control window.
//
// Chunks and events share one sequence space per job, so a single watermark
// covers both and replay after a reconnect is totally ordered. The schema makes
// that a primary key, so a redelivered entry is a no-op rather than a duplicate.
func (s *Service) LogStream(stream runnerv1.RunnerService_LogStreamServer) error {
	ctx := stream.Context()

	runnerID, err := s.authenticate(ctx)
	if err != nil {
		return err
	}

	cfg := s.logStreamConfig()
	throttle := NewThrottler(uint64(cfg.MaxInflightChunks) * uint64(cfg.MaxLogChunkBytes))

	// Watermarks are acked periodically rather than per chunk: one ack per
	// chunk would double the message count on the busiest path in the system
	// for no benefit, since the ack is only a hint to trim the journal.
	pending := newLogBatch()
	ticker := time.NewTicker(cfg.AckInterval)
	defer ticker.Stop()

	acks := make(chan *logBatch, 1)
	done := make(chan error, 1)

	go func() {
		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case batch := <-acks:
				for jobID, seq := range batch.seqs {
					if err := stream.Send(&runnerv1.LogDown{
						Msg: &runnerv1.LogDown_Ack{
							Ack: &runnerv1.LogAck{
								JobId:    jobID.String(),
								AckedSeq: seq,
							},
						},
					}); err != nil {
						done <- err
						return
					}
					// Release the throttler's accounting for this job after a
					// successful Send, so a failed ack does not free the
					// inflight budget.
					throttle.Ack(jobID, batch.bytes[jobID])
				}
			}
		}
	}()

	flush := func() {
		if len(pending.seqs) == 0 {
			return
		}
		batch := pending
		pending = newLogBatch()
		select {
		case acks <- batch:
			// nothing; the sender reads bytes from the batch directly
		default:
			// The ack sender is busy; fold this batch into the next one rather
			// than blocking ingest on it.
			for k, v := range batch.seqs {
				if v > pending.seqs[k] {
					pending.seqs[k] = v
				}
			}
			for k, v := range batch.bytes {
				pending.bytes[k] += v
			}
		}
	}

	for {
		select {
		case err := <-done:
			flush()
			return err
		case <-ticker.C:
			flush()
		default:
		}

		up, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			flush()
			return nil
		}
		if err != nil {
			return err
		}

		jobID, seq, nBytes, err := s.ingestLogUp(ctx, runnerID, up, &cfg, throttle)
		if err != nil {
			return err
		}
		if jobID != uuid.Nil && seq > 0 {
			pending.add(jobID, seq, nBytes)

			// Flush opportunistically so a short burst is acked promptly rather
			// than waiting out the full tick. This also covers the case where the
			// job finishes inside one tick: the trailing chunks get a chance to
			// reach Postgres before the runner closes its stream.
			if len(pending.seqs) > 0 {
				select {
				case <-ticker.C:
					flush()
				default:
				}
			}
		}
	}
}

// logStreamConfig derives the LogStream config from the service, applying
// defaults when the caller left a field zero.
func (s *Service) logStreamConfig() LogStreamConfig {
	cfg := DefaultLogStreamConfig()
	if s.cfg.MaxLogChunkBytes > 0 {
		cfg.MaxLogChunkBytes = s.cfg.MaxLogChunkBytes
	}
	if s.cfg.MaxInflightChunks > 0 {
		cfg.MaxInflightChunks = s.cfg.MaxInflightChunks
	}
	if s.cfg.AckInterval > 0 {
		cfg.AckInterval = s.cfg.AckInterval
	}
	return cfg
}

// ingestLogUp persists one chunk or event. It returns the job, sequence and
// byte count so the caller can track the watermark and the throttler.
func (s *Service) ingestLogUp(
	ctx context.Context,
	runnerID uuid.UUID,
	up *runnerv1.LogUp,
	cfg *LogStreamConfig,
	throttle *Throttler,
) (uuid.UUID, uint64, uint64, error) {
	switch m := up.Msg.(type) {
	case *runnerv1.LogUp_Chunk:
		chunk := m.Chunk
		if uint32(len(chunk.GetData())) > cfg.MaxLogChunkBytes {
			return uuid.Nil, 0, 0, status.Errorf(codes.InvalidArgument,
				"log chunk %d bytes exceeds max %d", len(chunk.GetData()), cfg.MaxLogChunkBytes)
		}
		jobID, leaseID, ok := parseJobLease(chunk.GetJobId(), chunk.GetLeaseId())
		if !ok {
			return uuid.Nil, 0, 0, nil
		}
		if err := s.assertLease(ctx, jobID, leaseID, runnerID); err != nil {
			// A chunk against a dead lease is dropped, not fatal: a runner that
			// has not yet processed its ABANDON verdict may still have some in
			// flight.
			//nolint:nilerr // dropping unauthorized input is the intended outcome
			return uuid.Nil, 0, 0, nil
		}
		// Throttle accounting: register the bytes we are about to persist.
		// ShouldThrottle is sticky; the session loop reads it to emit Throttle.
		throttle.Account(jobID, uint64(len(chunk.GetData())))

		// CIC-31: redaction runs HERE, on ingest, never at render time.
		// Since CIC-79, the masker's registered-value set is populated when a
		// runner claims a job whose secret_refs list is non-empty: the server
		// registers each delivered value with the service-wide masker before
		// returning it to the runner. The full lifecycle of masker values
		// (unregister on job completion or lease expiry) is a follow-up ticket
		// (CIC-119): registered values stay live in the masker for the
		// process lifetime even after a job completes or its lease
		// expires.  This is a minor secret-retention issue; the
		// process-level risk is low for a single-tenant deploy.
		data := s.cfg.Masker.Mask(chunk.GetData())
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO log_entries (job_id, seq, kind, step_index, io, data, runner_ts)
			VALUES ($1, $2, 'chunk', $3, $4, $5, $6)
			ON CONFLICT (job_id, seq) DO NOTHING`,
			jobID, int64(chunk.GetSeq()), int32(chunk.GetStepIndex()),
			ioToString(chunk.GetIo()), data, timeOrNil(chunk.GetRunnerTs())); err != nil {
			return uuid.Nil, 0, 0, status.Error(codes.Internal, "persist log chunk")
		}
		// CIC-33: fan out to SSE subscribers before the watermark advances,
		// so a client that reconnects after this watermark sees this entry
		// in the replay phase, not the live phase.
		if s.cfg.LogPublish != nil {
			s.cfg.LogPublish(jobID, "chunk", int64(chunk.GetSeq()))
		}
		if err := s.advanceLogWatermark(ctx, jobID, leaseID, chunk.GetSeq()); err != nil {
			return uuid.Nil, 0, 0, err
		}
		return jobID, chunk.GetSeq(), uint64(len(chunk.GetData())), nil

	case *runnerv1.LogUp_Event:
		event := m.Event
		jobID, leaseID, ok := parseJobLease(event.GetJobId(), event.GetLeaseId())
		if !ok {
			return uuid.Nil, 0, 0, nil
		}
		if err := s.assertLease(ctx, jobID, leaseID, runnerID); err != nil {
			// As above: an event against a dead lease is dropped, not fatal.
			//nolint:nilerr // dropping unauthorized input is the intended outcome
			return uuid.Nil, 0, 0, nil
		}

		// Structured events are tiny by design; we don't throttle them through
		// the Throttler, only log chunks.
		payload := event.GetJson()
		if payload == "" {
			payload = "{}"
		}
		// CIC-31: scrub the JSON body before persistence -- a token may be
		// embedded in any JSON string field.
		payload = string(s.cfg.Masker.Mask([]byte(payload)))
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO log_entries (job_id, seq, kind, step_index, event_kind, event_json, runner_ts)
			VALUES ($1, $2, 'event', $3, $4, $5::jsonb, $6)
			ON CONFLICT (job_id, seq) DO NOTHING`,
			jobID, int64(event.GetSeq()), int32(event.GetStepIndex()),
			event.GetKind(), payload, nil); err != nil {
			return uuid.Nil, 0, 0, status.Error(codes.Internal, "persist structured event")
		}
		if s.cfg.LogPublish != nil {
			s.cfg.LogPublish(jobID, "event", int64(event.GetSeq()))
		}
		if err := s.advanceLogWatermark(ctx, jobID, leaseID, event.GetSeq()); err != nil {
			return uuid.Nil, 0, 0, err
		}
		return jobID, event.GetSeq(), uint64(len(payload)), nil

	default:
		return uuid.Nil, 0, 0, nil
	}
}

func (s *Service) advanceLogWatermark(ctx context.Context, jobID, leaseID uuid.UUID, seq uint64) error {
	// Matches assertLease: a trailing chunk accepted after completion must move
	// the watermark too, or a reconnecting runner would replay it forever.
	if _, err := s.pool.Exec(ctx, `
		UPDATE jobs SET log_watermark = greatest(log_watermark, $2)
		   WHERE id = $1 AND (lease_id = $3 OR last_lease_id = $3)`,
		jobID, int64(seq), leaseID); err != nil {
		return status.Error(codes.Internal, "advance log watermark")
	}
	return nil
}

// trailingLogGrace is how long after a job finishes the server still accepts
// log entries produced under the lease that completed it.
//
// Logs and control messages travel on separate gRPC streams by design, so that
// a burst of output cannot delay a cancellation. The cost of that separation is
// that the two have no ordering guarantee relative to each other: a job's last
// chunks routinely arrive after its terminal status. Rejecting them would drop
// the final lines of output on every job — the ones that say why it failed.
const trailingLogGrace = 5 * time.Minute

// assertLease is the authorization check on the bulk lane. Without it, a runner
// with a valid credential could write log entries into another runner's job.
//
// It accepts two cases: the live lease, and the lease that just completed the
// job, within trailingLogGrace. What it never accepts is a lease belonging to a
// different runner, or one superseded by a requeue — a runner returning from a
// partition cannot append to the output of the run that replaced it.
func (s *Service) assertLease(ctx context.Context, jobID, leaseID, runnerID uuid.UUID) error {
	var ok bool
	if err := s.pool.QueryRow(ctx, `
		SELECT exists(
			SELECT 1 FROM jobs
			 WHERE id = $1
			   AND (
			          (lease_id = $2 AND claimed_by = $3 AND status IN ('claimed', 'running'))
			      OR (last_lease_id = $2 AND finished_at IS NOT NULL
			          AND finished_at > now() - $4::interval))
			    )`,
		jobID, leaseID, runnerID, trailingLogGrace.String()).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("runnersvc: lease is not valid for this job")
	}
	return nil
}

func parseJobLease(jobRaw, leaseRaw string) (uuid.UUID, uuid.UUID, bool) {
	jobID, err := uuid.Parse(jobRaw)
	if err != nil {
		return uuid.Nil, uuid.Nil, false
	}
	leaseID, err := uuid.Parse(leaseRaw)
	if err != nil {
		return uuid.Nil, uuid.Nil, false
	}
	return jobID, leaseID, true
}

func ioToString(io runnerv1.LogChunk_Io) string {
	if io == runnerv1.LogChunk_STDERR {
		return "stderr"
	}
	return "stdout"
}

func timeOrNil(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}

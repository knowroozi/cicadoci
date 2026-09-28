package runnersvc

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
)

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

	// Watermarks are acked periodically rather than per chunk: one ack per
	// chunk would double the message count on the busiest path in the system
	// for no benefit, since the ack is only a hint to trim the journal.
	const ackInterval = 250 * time.Millisecond
	pending := make(map[uuid.UUID]uint64)
	ticker := time.NewTicker(ackInterval)
	defer ticker.Stop()

	acks := make(chan map[uuid.UUID]uint64, 1)
	done := make(chan error, 1)

	go func() {
		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case batch := <-acks:
				for jobID, seq := range batch {
					if err := stream.Send(&runnerv1.LogDown{
						Msg: &runnerv1.LogDown_Ack{Ack: &runnerv1.LogAck{
							JobId:    jobID.String(),
							AckedSeq: seq,
						}},
					}); err != nil {
						done <- err
						return
					}
				}
			}
		}
	}()

	flush := func() {
		if len(pending) == 0 {
			return
		}
		batch := pending
		pending = make(map[uuid.UUID]uint64)
		select {
		case acks <- batch:
		default:
			// The ack sender is busy; fold this batch into the next one rather
			// than blocking ingest on it.
			for k, v := range batch {
				if v > pending[k] {
					pending[k] = v
				}
			}
		}
	}

	for {
		select {
		case err := <-done:
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

		jobID, seq, err := s.ingestLogUp(ctx, runnerID, up)
		if err != nil {
			return err
		}
		if jobID != uuid.Nil && seq > pending[jobID] {
			pending[jobID] = seq
		}

		// Flush opportunistically so a short burst is acked promptly rather
		// than waiting out the full tick.
		if len(pending) > 0 {
			select {
			case <-ticker.C:
				flush()
			default:
			}
		}
	}
}

// ingestLogUp persists one chunk or event. It returns the job and sequence so
// the caller can track the watermark.
func (s *Service) ingestLogUp(ctx context.Context, runnerID uuid.UUID, up *runnerv1.LogUp) (uuid.UUID, uint64, error) {
	switch m := up.Msg.(type) {
	case *runnerv1.LogUp_Chunk:
		chunk := m.Chunk
		jobID, leaseID, ok := parseJobLease(chunk.GetJobId(), chunk.GetLeaseId())
		if !ok {
			return uuid.Nil, 0, nil
		}
		if err := s.assertLease(ctx, jobID, leaseID, runnerID); err != nil {
			// A chunk against a dead lease is dropped, not fatal: a runner that
			// has not yet processed its ABANDON verdict may still have some in
			// flight. Tearing down the stream would punish a runner for a race
			// the protocol creates on purpose.
			//nolint:nilerr // dropping unauthorized input is the intended outcome
			return uuid.Nil, 0, nil
		}

		// CIC-31: redaction runs HERE, on ingest, never at render time. This
		// is the chokepoint every chunk passes through before persistence.
		// TODO(CIC-37): masker scrubs against an empty registered-value set
		// until the secrets store provides values to feed Register().
		data := s.cfg.Masker.Mask(chunk.GetData())
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO log_entries (job_id, seq, kind, step_index, io, data, runner_ts)
			VALUES ($1, $2, 'chunk', $3, $4, $5, $6)
			ON CONFLICT (job_id, seq) DO NOTHING`,
			jobID, int64(chunk.GetSeq()), int32(chunk.GetStepIndex()),
			ioToString(chunk.GetIo()), data, timeOrNil(chunk.GetRunnerTs())); err != nil {
			return uuid.Nil, 0, status.Error(codes.Internal, "persist log chunk")
		}
		if err := s.advanceLogWatermark(ctx, jobID, leaseID, chunk.GetSeq()); err != nil {
			return uuid.Nil, 0, err
		}
		return jobID, chunk.GetSeq(), nil

	case *runnerv1.LogUp_Event:
		event := m.Event
		jobID, leaseID, ok := parseJobLease(event.GetJobId(), event.GetLeaseId())
		if !ok {
			return uuid.Nil, 0, nil
		}
		if err := s.assertLease(ctx, jobID, leaseID, runnerID); err != nil {
			// As above: an event against a dead lease is dropped, not fatal.
			//nolint:nilerr // dropping unauthorized input is the intended outcome
			return uuid.Nil, 0, nil
		}

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
			// StructuredEvent carries no runner_ts in the frozen proto; the
			// server's received_at is the only timestamp for an event.
			event.GetKind(), payload, nil); err != nil {
			return uuid.Nil, 0, status.Error(codes.Internal, "persist structured event")
		}
		if err := s.advanceLogWatermark(ctx, jobID, leaseID, event.GetSeq()); err != nil {
			return uuid.Nil, 0, err
		}
		return jobID, event.GetSeq(), nil

	default:
		return uuid.Nil, 0, nil
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
			          AND finished_at > now() - $4::interval)
			   ))`,
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

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
	"cicadoci.com/cicado/server/queue"
)

// Session is one connected runner's control channel.
type Session struct {
	ID        string
	RunnerID  uuid.UUID
	Labels    []string
	Executors []string
	Capacity  int

	// outbound carries server -> runner messages. The stream is written by a
	// single goroutine (the Session handler); everything else enqueues here,
	// because a gRPC stream must not be written from two goroutines at once.
	outbound chan *runnerv1.ServerMessage

	mu       sync.Mutex
	inflight map[uuid.UUID]bool
	closed   bool
}

// Send queues a message to the runner. It reports false if the session is gone
// or its outbound buffer is full — in either case the dispatcher should treat
// the assignment as undelivered and let the job stay queued.
func (s *Session) Send(msg *runnerv1.ServerMessage) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()

	select {
	case s.outbound <- msg:
		return true
	default:
		return false
	}
}

// TrackAssigned records a job as in flight on this session.
func (s *Session) TrackAssigned(jobID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight[jobID] = true
}

// Release drops a job from the in-flight set.
func (s *Session) Release(jobID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, jobID)
}

// Available reports how many more jobs this runner can take.
func (s *Session) Available() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Capacity - len(s.inflight)
}

func (s *Session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.outbound)
	}
}

// SessionRegistry tracks the runners connected to THIS server instance.
//
// It is deliberately in-process, not shared state in Postgres. A runner's
// stream terminates on exactly one server, so only that server can push to it,
// and each instance dispatches only to its own runners. Because the claim is
// atomic, instances never contend for the same job — the fleet scales out
// horizontally without a coordination mechanism.
type SessionRegistry struct {
	mu       sync.RWMutex
	sessions map[uuid.UUID]*Session
}

// NewSessionRegistry builds an empty registry.
func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{sessions: make(map[uuid.UUID]*Session)}
}

func (r *SessionRegistry) add(s *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A runner reconnecting while its previous stream is still registered
	// replaces it; the old stream is already dead or about to be.
	if existing, ok := r.sessions[s.RunnerID]; ok {
		existing.close()
	}
	r.sessions[s.RunnerID] = s
}

func (r *SessionRegistry) remove(s *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.sessions[s.RunnerID]; ok && current == s {
		delete(r.sessions, s.RunnerID)
	}
}

// Snapshot returns the currently connected sessions.
func (r *SessionRegistry) Snapshot() []*Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	return out
}

// Get returns the session for a runner, if it is connected here.
func (r *SessionRegistry) Get(runnerID uuid.UUID) (*Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[runnerID]
	return s, ok
}

// outboundBuffer is how many server messages may queue for a runner before the
// dispatcher is told to back off. Small on purpose: a deep buffer would let the
// server hand out assignments a struggling runner will never get to.
const outboundBuffer = 64

// Session is the long-lived control channel.
//
// The protocol requires the first client message to be Hello; anything else
// closes the stream with FAILED_PRECONDITION. Hello carries the jobs the runner
// believes it is still executing, and ServerHello answers each one with RESUME
// or ABANDON — that exchange is what makes a reconnect safe rather than a
// source of two runners executing the same job.
func (s *Service) Session(stream runnerv1.RunnerService_SessionServer) error {
	ctx := stream.Context()

	runnerID, err := s.authenticate(ctx)
	if err != nil {
		return err
	}

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.FailedPrecondition, "first session message must be Hello")
	}
	if hello.GetProtocolVersion() != 1 {
		return status.Errorf(codes.FailedPrecondition,
			"unsupported protocol version %d", hello.GetProtocolVersion())
	}

	labels, executors, capacity, err := s.loadRunner(ctx, runnerID)
	if err != nil {
		return err
	}

	sess := &Session{
		ID:        uuid.NewString(),
		RunnerID:  runnerID,
		Labels:    labels,
		Executors: executors,
		Capacity:  capacity,
		outbound:  make(chan *runnerv1.ServerMessage, outboundBuffer),
		inflight:  make(map[uuid.UUID]bool),
	}

	verdicts, err := s.resumeVerdicts(ctx, runnerID, hello.GetResume(), sess)
	if err != nil {
		return err
	}

	if err := stream.Send(&runnerv1.ServerMessage{
		Msg: &runnerv1.ServerMessage_ServerHello{ServerHello: &runnerv1.ServerHello{
			SessionId:                sess.ID,
			HeartbeatIntervalSeconds: uint32(s.cfg.HeartbeatInterval.Seconds()),
			Limits: &runnerv1.Limits{
				MaxLogChunkBytes:  s.cfg.MaxLogChunkBytes,
				MaxInflightChunks: s.cfg.MaxInflightChunks,
				JournalDiskBytes:  s.cfg.JournalDiskBytes,
			},
			ResumeVerdicts:     verdicts,
			MinSupportedRunner: s.cfg.MinSupportedRunner,
			LatestRunner:       s.cfg.LatestRunner,
		}},
	}); err != nil {
		return err
	}

	if err := s.markRunnerOnline(ctx, runnerID, sess.ID); err != nil {
		return err
	}

	s.sessions.add(sess)
	defer func() {
		s.sessions.remove(sess)
		sess.close()
		// Use a fresh context: the stream's is already cancelled by the time
		// this runs, and marking the runner offline is worth one more query.
		offlineCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.markRunnerOffline(offlineCtx, runnerID); err != nil {
			s.log.Warn("mark runner offline", "runner_id", runnerID, "error", err)
		}
	}()

	s.log.Info("runner session opened",
		"runner_id", runnerID, "session_id", sess.ID, "labels", labels, "capacity", capacity)

	// Writer: the only goroutine that writes to the stream.
	writerDone := make(chan error, 1)
	go func() {
		for msg := range sess.outbound {
			if err := stream.Send(msg); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()

	// Reader: this goroutine owns Recv.
	readErr := s.readSession(stream, sess)

	sess.close()
	<-writerDone

	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	return nil
}

func (s *Service) readSession(stream runnerv1.RunnerService_SessionServer, sess *Session) error {
	ctx := stream.Context()
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		switch m := msg.Msg.(type) {
		case *runnerv1.RunnerMessage_Hello:
			return status.Error(codes.FailedPrecondition, "Hello may only be sent once, first")

		case *runnerv1.RunnerMessage_JobAck:
			s.handleJobAck(ctx, sess, m.JobAck)

		case *runnerv1.RunnerMessage_StatusUpdate:
			if err := s.handleStatusUpdate(ctx, sess, m.StatusUpdate); err != nil {
				s.log.Error("apply status update", "runner_id", sess.RunnerID, "error", err)
			}

		case *runnerv1.RunnerMessage_Heartbeat:
			if err := s.handleHeartbeat(ctx, sess, m.Heartbeat); err != nil {
				s.log.Error("apply heartbeat", "runner_id", sess.RunnerID, "error", err)
			}

		default:
			// Unknown message: ignore. The protocol evolves additively, so a
			// newer runner may legitimately send something this server has
			// never heard of, and dropping the stream would be wrong.
			s.log.Debug("ignoring unknown session message", "runner_id", sess.RunnerID)
		}
	}
}

// handleJobAck records a runner's acceptance or rejection of an assignment.
// A rejection requeues immediately: the runner is telling us it cannot run this,
// so there is no point waiting for the lease to expire.
func (s *Service) handleJobAck(ctx context.Context, sess *Session, ack *runnerv1.JobAck) {
	jobID, err := uuid.Parse(ack.GetJobId())
	if err != nil {
		return
	}

	if ack.GetResult() == runnerv1.JobAck_ACCEPTED {
		s.log.Debug("job accepted", "job_id", jobID, "runner_id", sess.RunnerID)
		return
	}

	sess.Release(jobID)
	s.log.Info("job rejected by runner",
		"job_id", jobID, "runner_id", sess.RunnerID, "reason", ack.GetRejectReason())
	if _, err := s.queue.Requeue(ctx, jobID, "rejected: "+ack.GetRejectReason(), 0); err != nil {
		s.log.Error("requeue rejected job", "job_id", jobID, "error", err)
	}
}

// handleStatusUpdate persists a status update and applies terminal states.
//
// Persistence is keyed (job_id, seq) with ON CONFLICT DO NOTHING, so a runner
// replaying its journal after a reconnect cannot double-apply anything.
func (s *Service) handleStatusUpdate(ctx context.Context, sess *Session, upd *runnerv1.StatusUpdate) error {
	// A malformed identifier is a broken client, not a server error. Drop the
	// message rather than killing a session over one bad field.
	jobID, err := uuid.Parse(upd.GetJobId())
	if err != nil {
		//nolint:nilerr // a malformed id is dropped, not escalated
		return nil
	}
	leaseID, err := uuid.Parse(upd.GetLeaseId())
	if err != nil {
		//nolint:nilerr // a malformed id is dropped, not escalated
		return nil
	}

	state := statusStateToString(upd.GetState())
	if state == "" {
		return nil
	}

	var runnerTS *time.Time
	if ts := upd.GetRunnerTs(); ts != nil {
		t := ts.AsTime()
		runnerTS = &t
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO job_status_updates
			(job_id, seq, lease_id, state, step_index, exit_code, error_class, detail, runner_ts)
		VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''), $9)
		ON CONFLICT (job_id, seq) DO NOTHING`,
		jobID, upd.GetSeq(), leaseID, state, int32(upd.GetStepIndex()),
		upd.GetExitCode(), upd.GetErrorClass(), upd.GetDetail(), runnerTS); err != nil {
		return err
	}

	// Advance the watermark so a reconnect replays only what we have not seen.
	// last_lease_id is accepted as well: the terminal update itself clears
	// lease_id, so the update that finishes a job would otherwise never be
	// reflected in its own watermark.
	if _, err := s.pool.Exec(ctx, `
		UPDATE jobs SET status_watermark = greatest(status_watermark, $2)
		 WHERE id = $1 AND (lease_id = $3 OR last_lease_id = $3)`,
		jobID, int64(upd.GetSeq()), leaseID); err != nil {
		return err
	}

	switch upd.GetState() {
	case runnerv1.StatusUpdate_RUNNING_STEP:
		if err := s.queue.MarkRunning(ctx, jobID, leaseID); err != nil && !errors.Is(err, queue.ErrStaleLease) {
			return err
		}
	case runnerv1.StatusUpdate_SUCCEEDED, runnerv1.StatusUpdate_FAILED, runnerv1.StatusUpdate_CANCELLED:
		terminal := map[runnerv1.StatusUpdate_State]queue.TerminalState{
			runnerv1.StatusUpdate_SUCCEEDED: queue.StateSucceeded,
			runnerv1.StatusUpdate_FAILED:    queue.StateFailed,
			runnerv1.StatusUpdate_CANCELLED: queue.StateCancelled,
		}[upd.GetState()]

		err := s.queue.Complete(ctx, queue.Completion{
			JobID:       jobID,
			LeaseID:     leaseID,
			State:       terminal,
			ExitCode:    upd.GetExitCode(),
			ErrorClass:  upd.GetErrorClass(),
			ErrorDetail: upd.GetDetail(),
		})
		// A stale lease here is expected, not exceptional: it means this runner
		// came back from a partition after the job was reassigned. Rejecting the
		// report is the whole point.
		if errors.Is(err, queue.ErrStaleLease) {
			s.log.Info("dropped terminal status against a stale lease",
				"job_id", jobID, "runner_id", sess.RunnerID)
		} else if err != nil {
			return err
		}
		sess.Release(jobID)

		s.log.Info("job finished",
			"job_id", jobID, "runner_id", sess.RunnerID,
			"state", state, "exit_code", upd.GetExitCode())
	}

	// Acknowledge so the runner can trim its journal.
	sess.Send(&runnerv1.ServerMessage{
		Msg: &runnerv1.ServerMessage_StatusAck{StatusAck: &runnerv1.StatusAck{
			JobId:    jobID.String(),
			AckedSeq: upd.GetSeq(),
		}},
	})
	return nil
}

// handleHeartbeat renews leases and tells the runner to abandon anything it has
// lost. A runner that was partitioned long enough to be swept must stop, rather
// than keep running work that now belongs to someone else.
func (s *Service) handleHeartbeat(ctx context.Context, sess *Session, hb *runnerv1.Heartbeat) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE runners SET last_heartbeat_at = now() WHERE id = $1`, sess.RunnerID); err != nil {
		return err
	}

	active := make([]uuid.UUID, 0, len(hb.GetActiveJobIds()))
	for _, raw := range hb.GetActiveJobIds() {
		if id, err := uuid.Parse(raw); err == nil {
			active = append(active, id)
		}
	}
	if len(active) == 0 {
		return nil
	}

	abandon, err := s.queue.RenewLease(ctx, sess.RunnerID, active, s.cfg.LeaseTTL())
	if err != nil {
		return err
	}
	for _, jobID := range abandon {
		s.log.Info("telling runner to abandon a lost job",
			"job_id", jobID, "runner_id", sess.RunnerID)
		sess.Release(jobID)
		sess.Send(&runnerv1.ServerMessage{
			Msg: &runnerv1.ServerMessage_CancelJob{CancelJob: &runnerv1.CancelJob{
				JobId:        jobID.String(),
				Reason:       "lease no longer valid",
				GraceSeconds: 0,
			}},
		})
	}
	return nil
}

// resumeVerdicts answers each job the runner says it is still executing.
//
// RESUME means the lease is still ours and the runner should carry on, replaying
// from the server's watermarks. ABANDON means the lease is gone — expired,
// reassigned, or cancelled — and the runner must kill local execution and
// discard that job's journal without reporting anything further.
func (s *Service) resumeVerdicts(ctx context.Context, runnerID uuid.UUID, states []*runnerv1.ResumeState, sess *Session) ([]*runnerv1.ResumeVerdict, error) {
	verdicts := make([]*runnerv1.ResumeVerdict, 0, len(states))

	for _, st := range states {
		jobID, err := uuid.Parse(st.GetJobId())
		if err != nil {
			continue
		}
		leaseID, err := uuid.Parse(st.GetLeaseId())
		if err != nil {
			verdicts = append(verdicts, abandonVerdict(st.GetJobId()))
			continue
		}

		var (
			logWatermark    int64
			statusWatermark int64
			valid           bool
		)
		err = s.pool.QueryRow(ctx, `
			SELECT log_watermark, status_watermark, true
			  FROM jobs
			 WHERE id = $1 AND lease_id = $2 AND claimed_by = $3
			   AND status IN ('claimed', 'running')
			   AND lease_expires_at > now()`,
			jobID, leaseID, runnerID).Scan(&logWatermark, &statusWatermark, &valid)
		if err != nil || !valid {
			verdicts = append(verdicts, abandonVerdict(st.GetJobId()))
			continue
		}

		sess.TrackAssigned(jobID)
		verdicts = append(verdicts, &runnerv1.ResumeVerdict{
			JobId:           jobID.String(),
			Verdict:         runnerv1.ResumeVerdict_RESUME,
			LogWatermark:    uint64(logWatermark),
			StatusWatermark: uint64(statusWatermark),
		})
	}
	return verdicts, nil
}

func abandonVerdict(jobID string) *runnerv1.ResumeVerdict {
	return &runnerv1.ResumeVerdict{JobId: jobID, Verdict: runnerv1.ResumeVerdict_ABANDON}
}

func (s *Service) loadRunner(ctx context.Context, runnerID uuid.UUID) (labels, executors []string, capacity int, err error) {
	var cap32 int32
	if err := s.pool.QueryRow(ctx,
		`SELECT labels, executors, capacity FROM runners WHERE id = $1`, runnerID).
		Scan(&labels, &executors, &cap32); err != nil {
		return nil, nil, 0, status.Error(codes.Internal, "load runner")
	}
	return labels, executors, int(cap32), nil
}

func (s *Service) markRunnerOnline(ctx context.Context, runnerID uuid.UUID, sessionID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE runners
		   SET status = 'online', last_session_id = $2,
		       last_heartbeat_at = now(), updated_at = now()
		 WHERE id = $1`, runnerID, sessionID)
	return err
}

func (s *Service) markRunnerOffline(ctx context.Context, runnerID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE runners SET status = 'offline', updated_at = now() WHERE id = $1`, runnerID)
	return err
}

func statusStateToString(state runnerv1.StatusUpdate_State) string {
	switch state {
	case runnerv1.StatusUpdate_PREPARING:
		return "preparing"
	case runnerv1.StatusUpdate_RUNNING_STEP:
		return "running_step"
	case runnerv1.StatusUpdate_UPLOADING:
		return "uploading"
	case runnerv1.StatusUpdate_SUCCEEDED:
		return "succeeded"
	case runnerv1.StatusUpdate_FAILED:
		return "failed"
	case runnerv1.StatusUpdate_CANCELLED:
		return "cancelled"
	default:
		return ""
	}
}

// AssignmentFor builds a JobAssignment from a claimed job.
//
// Note what does not travel here: secret values. The assignment carries secret
// NAMES; values come from GetJobSecrets against the live lease. An assignment
// may be redelivered after a reconnect, and anything redeliverable must be safe
// to redeliver.
func AssignmentFor(job queue.ClaimedJob) *runnerv1.ServerMessage {
	steps := make([]*runnerv1.StepSpec, 0, len(job.Steps))
	for _, st := range job.Steps {
		steps = append(steps, &runnerv1.StepSpec{
			Index:          uint32(st.Index),
			Name:           st.Name,
			Image:          st.Image,
			Command:        st.Command,
			Env:            st.Env,
			TimeoutSeconds: uint32(st.TimeoutSeconds),
			NetworkEnabled: st.NetworkEnabled,
			// CIC-118: carry the `if:` conditional so the runner can enforce it.
			IfCondition: st.If,
		})
	}

	return &runnerv1.ServerMessage{
		Msg: &runnerv1.ServerMessage_JobAssignment{JobAssignment: &runnerv1.JobAssignment{
			JobId:          job.JobID.String(),
			LeaseId:        job.LeaseID.String(),
			LeaseExpiresAt: timestamppb.New(job.LeaseExpiresAt),
			Spec: &runnerv1.JobSpec{
				RunId:          job.RunID.String(),
				WorkflowName:   job.WorkflowName,
				Steps:          steps,
				Env:            job.Env,
				SecretRefs:     job.SecretRefs,
				TimeoutSeconds: uint32(job.TimeoutSeconds),
				Executor:       job.Executor,
				Git: &runnerv1.GitContext{
					CloneUrl: job.GitCloneURL,
					Ref:      job.GitRef,
					Sha:      job.GitSHA,
				},
			},
		}},
	}
}

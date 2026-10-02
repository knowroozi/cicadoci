// Package runner is the runner agent: it enrolls with a server, opens the
// control and log streams, and executes assigned jobs.
//
// Everything is outbound. The runner dials the server; the server never dials
// back. That is what lets a runner sit behind NAT in a customer network with no
// inbound firewall rules (ADR-004).
//
// Transport security is the caller's responsibility: New takes an already-
// established *grpc.ClientConn, and this package has no opinion on whether it
// carries TLS. cmd/cicado-runner supplies TLS by default and refuses a plaintext
// connection unless it is passed an explicit -insecure flag — a runner talking
// plaintext to a server outside a fully trusted network exposes job output and
// its own credential in the clear.
//
// A dropped connection is not fatal: Run reconnects on a jittered backoff and
// asks the server for resume verdicts, and a SIGTERM/SIGINT triggers a graceful
// drain — in-flight jobs are given a grace window to finish before being
// cancelled, rather than being cancelled the instant the signal arrives.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
	"cicadoci.com/cicado/runner/journal"
)

// Version is the runner build version reported in Capabilities.
const Version = "0.1.0"

// Config configures an agent.
type Config struct {
	// ServerAddress is the gRPC endpoint, host:port.
	ServerAddress string
	// Credential is the runner credential from enrollment.
	Credential string
	// RunnerID is this runner's server-assigned identity.
	RunnerID string
	// WorkspaceRoot is where job workspaces are created.
	WorkspaceRoot string
	// Capacity is how many jobs may run concurrently.
	Capacity int
	// Executors this runner offers.
	Executors []string
	// DrainTimeout bounds how long a graceful shutdown waits for in-flight jobs to
	// finish on their own before it cancels them. Zero cancels them immediately
	// (the fast, deterministic default used by the drain unit tests).
	DrainTimeout time.Duration
	Logger       *slog.Logger
}

// Agent is a connected runner.
type Agent struct {
	cfg    Config
	conn   *grpc.ClientConn
	client runnerv1.RunnerServiceClient
	// secrets is the narrow lease-scoped GetJobSecrets client used to fetch
	// secret VALUES for a job at execute time (CIC-79). nil when the agent was
	// constructed without a live gRPC connection (e.g. unit tests); when nil,
	// executeJob delivers an empty secret set instead of panicking on a broken
	// client.
	secrets secretsClient
	log     *slog.Logger

	// seq is the per-job sequence allocator for the log stream. Chunks and
	// events share one space per the protocol, so one counter serves both.
	seqMu sync.Mutex
	seq   map[string]*atomic.Uint64

	logMu     sync.Mutex
	logStream runnerv1.RunnerService_LogStreamClient

	jobsMu sync.Mutex
	active map[string]*activeJob

	statusSeq atomic.Uint64

	// jobCtx is the parent of every in-flight job's context. It lives for the
	// whole Run, not per session, so a transient reconnect never cancels work the
	// runner may resume (see Run); only a drain ends it.
	jobCtx    context.Context
	jobCancel context.CancelFunc

	// draining stops the agent from accepting new work once a shutdown has begun.
	draining atomic.Bool

	// drainWG counts in-flight jobs so a drain can wait for them within a grace
	// window before cancelling whatever is still running.
	drainWG sync.WaitGroup

	// journal tracks unacked outbound messages (both lanes, all jobs) so that
	// a RESUME verdict replay can re-send entries from the server's
	// high-water mark.  Trimmed by StatusAck (LaneStatus) and LogAck
	// (LaneLog) as they arrive.  M1 implementation (MemoryJournal) is
	// in-process; a disk-backed journal for crash survival is deferred.
	journal *journal.MemoryJournal
}

type activeJob struct {
	leaseID string
	cancel  context.CancelFunc
}

// Enroll registers with a server using a one-time registration token and
// returns the credential and identity to persist.
//
// Labels in the response are authoritative and come from the token, not from
// anything this process asked for.
func Enroll(ctx context.Context, conn *grpc.ClientConn, registrationToken, name string, executors []string) (*runnerv1.RegisterResponse, error) {
	client := runnerv1.NewRunnerServiceClient(conn)
	resp, err := client.Register(ctx, &runnerv1.RegisterRequest{
		RegistrationToken: registrationToken,
		Name:              name,
		Capabilities:      capabilities(executors),
	})
	if err != nil {
		return nil, fmt.Errorf("runner: register: %w", err)
	}
	return resp, nil
}

func capabilities(executors []string) *runnerv1.Capabilities {
	return &runnerv1.Capabilities{
		Executors:     executors,
		Os:            runtime.GOOS,
		Arch:          runtimeArch(),
		RunnerVersion: Version,
		Features:      []string{},
	}
}

// New builds an agent over an established connection.
func New(cfg Config, conn *grpc.ClientConn) *Agent {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.Capacity <= 0 {
		cfg.Capacity = 1
	}
	return &Agent{
		cfg:    cfg,
		conn:   conn,
		client: runnerv1.NewRunnerServiceClient(conn),
		// fetchJobSecrets degrades to an empty set on a nil client, so we only
		// wire the secrets client when a live connection exists. Production
		// always has one; unit-test agents (conn==nil) leave it nil on purpose.
		secrets: maybeSecretsClient(conn),
		log:     log,
		seq:     make(map[string]*atomic.Uint64),
		active:  make(map[string]*activeJob),
		journal: journal.NewMemoryJournal(),
	}
}

// maybeSecretsClient returns the lease-scoped GetJobSecrets client when a live
// gRPC connection is present, and nil otherwise. executeJob treats a nil client
// as "no secrets" so agent unit tests that bypass the connection still build.
func maybeSecretsClient(conn *grpc.ClientConn) secretsClient {
	if conn == nil {
		return nil
	}
	return runnerv1.NewRunnerServiceClient(conn)
}

// authContext attaches the runner credential to outgoing calls.
func (a *Agent) authContext(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+a.cfg.Credential)
}

// Run serves the runner until ctx is cancelled, reconnecting on transient
// failures and draining in-flight work on the way out.
//
// A dropped connection is not an error to give up on: runners live on spot
// instances and flaky networks, and the protocol is built so that reconnecting
// and asking for resume verdicts is the normal recovery path.
//
// Two contexts keep the connection and the jobs on different lifetimes. The
// session's own context derives from ctx, so a signal tears it down promptly;
// but the jobs a session spawns derive from a separate jobCtx that lives for the
// whole Run, so a transient disconnect never cancels work the runner may resume
// — the loop reconnects and the server answers each resume with a verdict. Only
// a drain ends jobCtx, and even then after a grace window (see drain).
//
// The retry loop reuses a.conn (and therefore a.client) across iterations rather
// than dialing a new *grpc.ClientConn each time — that conn manages its own
// transport recovery, so redialing would fight it. What this loop retries is the
// RPC layer: a.session(ctx, jobCtx) opens a fresh Session/LogStream pair each
// pass.
func (a *Agent) Run(ctx context.Context) error {
	jobCtx, jobCancel := context.WithCancel(context.Background())
	a.jobCtx = jobCtx
	a.jobCancel = jobCancel
	defer jobCancel()

	// On any exit, drain in-flight work within the grace window before the
	// process goes away. It is deferred ahead of jobCancel so it runs first
	// (LIFO) and cancels only the stragglers that outlived the grace.
	defer a.drain(a.cfg.DrainTimeout)

	// Reconnect backoff schedule: a 1 s base, full-jittered up to the cap. The 60 s
	// cap matches the protocol's "jittered exp backoff (1 s -> 60 s cap)" for a
	// server restart (project-docs/04-runner-protocol.md §9); it is inherited from
	// that FROZEN spec, not a locally-tuned number.
	const (
		baseBackoff = time.Second
		maxBackoff  = 60 * time.Second
	)

	for attempt := 0; ; attempt++ {
		// A stop signal tears the job-independent job context down in the deferred
		// drain, so a cancelled ctx is the cue to stop reconnecting and let that
		// drain finish rather than dialing another session.
		if err := ctx.Err(); err != nil {
			return err
		}

		err := a.session(ctx, jobCtx)
		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}
		if err != nil {
			a.log.Warn("session ended; reconnecting", "error", err, "attempt", attempt)
		}

		// A freshly drawn, full-jittered value so a fleet of runners that dropped
		// together does not stampede the server on identical retry ticks.
		sleep := jitteredBackoff(attempt, baseBackoff, maxBackoff, randJitter)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
	}
}

// session runs one connection's lifetime: Hello, ServerHello, then the message
// loop with a heartbeat ticker alongside.
func (a *Agent) session(stopCtx, jobCtx context.Context) error {
	// The session's own context derives from stopCtx, so a signal tears the
	// session down promptly; the jobs it spawns derive from jobCtx (see Run), so
	// a transient disconnect ends the session without cancelling their work.
	sessionCtx, cancel := context.WithCancel(stopCtx)
	defer cancel()

	authCtx := a.authContext(sessionCtx)

	stream, err := a.client.Session(authCtx)
	if err != nil {
		return fmt.Errorf("runner: open session: %w", err)
	}

	if err := stream.Send(&runnerv1.RunnerMessage{
		Msg: &runnerv1.RunnerMessage_Hello{Hello: &runnerv1.Hello{
			ProtocolVersion: 1,
			Capabilities:    capabilities(a.cfg.Executors),
			Resume:          a.resumeStates(),
		}},
	}); err != nil {
		return fmt.Errorf("runner: send hello: %w", err)
	}

	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("runner: receive server hello: %w", err)
	}
	serverHello := first.GetServerHello()
	if serverHello == nil {
		return errors.New("runner: first server message was not ServerHello")
	}
	// Server verdicts are applied after the send and log streams are
	// ready below, so that a RESUME replay has both lanes available.
	verdicts := serverHello.GetResumeVerdicts()

	heartbeat := time.Duration(serverHello.GetHeartbeatIntervalSeconds()) * time.Second
	if heartbeat <= 0 {
		heartbeat = 20 * time.Second
	}
	a.log.Info("session established",
		"session_id", serverHello.GetSessionId(),
		"heartbeat", heartbeat,
		"max_chunk_bytes", serverHello.GetLimits().GetMaxLogChunkBytes())

	// Open the bulk lane on the same connection.
	logStream, err := a.client.LogStream(authCtx)
	if err != nil {
		return fmt.Errorf("runner: open log stream: %w", err)
	}
	a.setLogStream(logStream)
	defer a.setLogStream(nil)

	// Drain log acks so the server's flow control is not blocked by an unread
	// stream.  Each ack trims the journal log lane up to the server's
	// acknowledged sequence so unacked entries can be replayed on RESUME.
	go func() {
		for {
			down, err := logStream.Recv()
			if err != nil {
				return
			}
			if ack := down.GetAck(); ack != nil {
				jobID := ack.GetJobId()
				ackedSeq := ack.GetAckedSeq()
				a.log.Debug("log ack: trimming journal", "job_id", jobID, "acked_seq", ackedSeq)
				a.journal.Trim(jobID, journal.LaneLog, ackedSeq)
			}
		}
	}()

	var sendMu sync.Mutex
	send := func(msg *runnerv1.RunnerMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(msg)
	}

	errc := make(chan error, 2)

	// Apply verdicts now that send and the log stream are ready: ABANDON
	// cancels each job immediately; RESUME replays unacked entries from
	// the journal starting at the server's high-water mark.
	a.applyResumeVerdicts(verdicts)

	// replayJournal acquires a.logMu for its whole loop so live log sends
	// (sendChunk / sendEvent → sendLogUp) are fenced until the replay
	// window closes, guaranteeing that replayed entries are on the wire
	// in ascending seq order relative to new live sends.
	//
	a.replayJournal(verdicts, send, func(up *runnerv1.LogUp) {
		// replayJournal holds logMu for its whole loop; no per-message lock here.
		if a.logStream == nil {
			return
		}
		if err := a.logStream.Send(up); err != nil {
			a.log.Debug("replay log chunk failed", "job_id", up.GetChunk().GetJobId(), "error", err)
		}
	})

	go func() { errc <- a.heartbeatLoop(sessionCtx, send, heartbeat) }()
	go func() { errc <- a.receiveLoop(sessionCtx, stream, send, jobCtx) }()

	err = <-errc
	cancel()
	return err
}

func (a *Agent) heartbeatLoop(ctx context.Context, send func(*runnerv1.RunnerMessage) error, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := send(&runnerv1.RunnerMessage{
				Msg: &runnerv1.RunnerMessage_Heartbeat{Heartbeat: &runnerv1.Heartbeat{
					ActiveJobIds: a.activeJobIDs(),
				}},
			}); err != nil {
				return fmt.Errorf("runner: send heartbeat: %w", err)
			}
		}
	}
}

func (a *Agent) receiveLoop(ctx context.Context, stream runnerv1.RunnerService_SessionClient, send func(*runnerv1.RunnerMessage) error, jobCtx context.Context) error {
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return errors.New("runner: server closed the session")
		}
		if err != nil {
			return fmt.Errorf("runner: receive: %w", err)
		}

		switch m := msg.Msg.(type) {
		case *runnerv1.ServerMessage_JobAssignment:
			// Jobs run on jobCtx, not this session's context, so a transient
			// disconnect that ends this loop does not cancel their execution.
			a.startJob(jobCtx, m.JobAssignment, send)

		case *runnerv1.ServerMessage_CancelJob:
			a.cancelJob(m.CancelJob)

		case *runnerv1.ServerMessage_StatusAck:
			// Trim the journal status lane up to the server's acknowledged
			// sequence so unacked entries that survive a transient disconnect
			// can be replayed on RESUME.
			a.journal.Trim(m.StatusAck.GetJobId(), journal.LaneStatus, m.StatusAck.GetAckedSeq())

		case *runnerv1.ServerMessage_Throttle:
			a.log.Info("server asked us to throttle",
				"job_id", m.Throttle.GetJobId(), "bytes_per_sec", m.Throttle.GetBytesPerSec())

		case *runnerv1.ServerMessage_UpdateNotice:
			a.log.Warn("runner update available",
				"latest", m.UpdateNotice.GetLatest(),
				"min_supported", m.UpdateNotice.GetMinSupported(),
				"message", m.UpdateNotice.GetMessage())

		default:
			// Additive evolution: a newer server may send something this runner
			// has never heard of, and that must not be fatal.
		}
	}
}

// jitteredBackoff is the pure core of the reconnect schedule: an exponentially
// expanding window under full jitter, so a fleet of runners that dropped at once
// does not retry on identical ticks. The window is base*2^attempt capped at max,
// and the value returned is a uniform draw in [0, window). rnd must honor that
// range; a zero draw is floored to base so a retry never busy-loops.
func jitteredBackoff(attempt int, base, max time.Duration, rnd func(window int64) int64) time.Duration {
	window := int64(base)
	for i := 0; i < attempt; i++ {
		window *= 2
		if window > int64(max) || window <= 0 {
			window = int64(max) // cap, and guard int64 overflow of a long attempt run
			break
		}
	}
	if sleep := int64(time.Duration(rnd(window))); sleep > 0 {
		return time.Duration(sleep)
	}
	return base
}

// randJitter is the production jitter source: a uniform value in [0, window) over
// math/rand/v2's global source. This is retry jitter, not a cryptographic use, so
// the runtime-seeded global is fine.
func randJitter(window int64) int64 { return rand.Int64N(window) }

// replayJournal re-sends any unacked status updates and log chunks recorded in
// the journal for the jobs the server told us to RESUME.  The server provides
// a per-job high-water mark (StatusAck.acked_seq / LogAck.acked_seq) and the
// journal replays from watermark+1; each subsequent ack trims so the window
// shrinks monotonically.
//
// func (a *Agent) replayJournal(...)
//
// M1 implementation is MemoryJournal: an empty journal after a process restart
// means nothing to replay, the lease expiry re-queues the job, and the server's
// idempotent upserts make that safe.
//
// logMu is held for the entire loop so live log sends from running jobs are
// fenced until the replay window closes. Because MemoryJournal's Replay
// returns a snapshot, the entries being replayed are a fixed set at call time;
// new records appended concurrently by the live path are NOT included in this
// replay and will flow through the live sendLogUp path (which also acquires
// logMu).  This ordering guarantee means the server never sees log entries out
// of seq order for a single job, even though the server's ON CONFLICT (job_id,
// seq) upsert would handle out-of-order arrivals harmlessly.
func (a *Agent) replayJournal(
	verdicts []*runnerv1.ResumeVerdict,
	send func(*runnerv1.RunnerMessage) error,
	logSender func(*runnerv1.LogUp),
) {
	// Fence live log-stream sends for the duration of this replay.  After this
	// returns, logMu is released and the live path can use it again.
	a.logMu.Lock()
	defer a.logMu.Unlock()

	for _, v := range verdicts {
		if v.GetVerdict() != runnerv1.ResumeVerdict_RESUME {
			continue
		}
		jobID := v.GetJobId()
		logWatermark := v.GetLogWatermark()
		statusWatermark := v.GetStatusWatermark()

		// Replay status lane.
		for _, e := range a.journal.Replay(jobID, journal.LaneStatus, statusWatermark) {
			su, ok := e.Msg.(*runnerv1.StatusUpdate)
			if !ok {
				continue
			}
			a.log.Debug("replaying status update", "job_id", jobID, "seq", su.GetSeq())
			if err := send(&runnerv1.RunnerMessage{
				Msg: &runnerv1.RunnerMessage_StatusUpdate{StatusUpdate: su},
			}); err != nil {
				a.log.Error("replay status failed", "job_id", jobID,
					"seq", su.GetSeq(), "error", err)
			}
		}

		// Replay log lane (chunks + structured events).
		// logSender is called without an extra lock: replayJournal holds logMu.
		for _, e := range a.journal.Replay(jobID, journal.LaneLog, logWatermark) {
			lg, ok := e.Msg.(*runnerv1.LogUp)
			if !ok {
				continue
			}
			a.log.Debug("replaying log entry", "job_id", jobID, "seq", e.Seq)
			logSender(lg)
		}
	}
}

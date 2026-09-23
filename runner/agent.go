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
	log    *slog.Logger

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
		log:    log,
		seq:    make(map[string]*atomic.Uint64),
		active: make(map[string]*activeJob),
	}
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

	const (
		baseBackoff = time.Second
		maxBackoff  = 30 * time.Second
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

	a.applyResumeVerdicts(serverHello.GetResumeVerdicts())

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
	// stream. The acks themselves only matter once there is a journal to trim.
	go func() {
		for {
			if _, err := logStream.Recv(); err != nil {
				return
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
			// Nothing to trim yet: the local journal lands with CIC-17.

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

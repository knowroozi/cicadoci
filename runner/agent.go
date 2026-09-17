// Package runner is the runner agent: it enrolls with a server, opens the
// control and log streams, and executes assigned jobs.
//
// Everything is outbound. The runner dials the server; the server never dials
// back. That is what lets a runner sit behind NAT in a customer network with no
// inbound firewall rules (ADR-004).
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	Logger    *slog.Logger
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
		Os:            "linux",
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

// Run opens both streams and serves until the context is cancelled.
//
// A dropped connection is not an error to give up on: runners live on spot
// instances and flaky networks, and the protocol is built so that reconnecting
// and asking for resume verdicts is the normal recovery path.
func (a *Agent) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := a.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			a.log.Warn("session ended; reconnecting", "error", err, "retry_in", backoff)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// session runs one connection's lifetime: Hello, ServerHello, then the message
// loop with a heartbeat ticker alongside.
func (a *Agent) session(ctx context.Context) error {
	sessionCtx, cancel := context.WithCancel(ctx)
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
	go func() { errc <- a.receiveLoop(sessionCtx, stream, send) }()

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

func (a *Agent) receiveLoop(ctx context.Context, stream runnerv1.RunnerService_SessionClient, send func(*runnerv1.RunnerMessage) error) error {
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
			a.startJob(ctx, m.JobAssignment, send)

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

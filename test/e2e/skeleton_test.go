// Package e2e holds the walking skeleton: a hardcoded job that travels the
// whole path — database, scheduler, real gRPC streams, runner, shell executor —
// and comes back.
//
// This is the M0 exit criterion (CIC-10). Its job is to prove the frozen wire
// format works end to end before any feature is built on top of it, and to fail
// loudly if a later change breaks the path. It uses no fakes for anything that
// crosses the wire: real Postgres, real gRPC over a real socket, real process
// execution.
package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"cicadoci.com/cicado/internal/testdb"
	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
	"cicadoci.com/cicado/runner"
	"cicadoci.com/cicado/server/auth"
	"cicadoci.com/cicado/server/dispatch"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/runnersvc"
	"cicadoci.com/cicado/server/store"
)

// harness is a running server plus the pieces a test needs to talk to it.
type harness struct {
	store      *store.Store
	queue      *queue.Postgres
	svc        *runnersvc.Service
	dispatcher *dispatch.Dispatcher
	addr       string
	log        *slog.Logger
}

// startServer brings up the gRPC service and dispatcher on a random port.
func startServer(t *testing.T, ctx context.Context) *harness {
	t.Helper()

	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// A short heartbeat keeps the test's lease behavior observable without
	// making it slow.
	cfg := runnersvc.DefaultConfig()
	cfg.HeartbeatInterval = 2 * time.Second

	svc := runnersvc.New(st.Pool(), q, cfg, log)

	dispatchCfg := dispatch.DefaultConfig(cfg.LeaseTTL())
	dispatchCfg.PollInterval = 250 * time.Millisecond
	dispatchCfg.SweepInterval = 500 * time.Millisecond
	dispatcher := dispatch.New(q, svc.Sessions(), dispatchCfg, log)

	var listenCfg net.ListenConfig
	listener, err := listenCfg.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	runnerv1.RegisterRunnerServiceServer(grpcServer, svc)

	go func() { _ = grpcServer.Serve(listener) }()
	go func() { _ = dispatcher.Run(ctx) }()
	go func() {
		_ = queue.Listen(ctx, st.DSN(), func(queue.QueuedNotification) { dispatcher.Wake() })
	}()

	t.Cleanup(grpcServer.Stop)

	return &harness{
		store:      st,
		queue:      q,
		svc:        svc,
		dispatcher: dispatcher,
		addr:       listener.Addr().String(),
		log:        log,
	}
}

// mintRegistrationToken creates an org and a registration token carrying labels.
func (h *harness) mintRegistrationToken(t *testing.T, labels []string) string {
	t.Helper()
	ctx := context.Background()

	var orgID uuid.UUID
	if err := h.store.Pool().QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ('acme', 'Acme') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("create org: %v", err)
	}

	token, err := auth.Generate(auth.KindRegistration)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	claims := fmt.Sprintf(`{"labels": %s}`, jsonStringArray(labels))
	if _, err := h.store.Pool().Exec(ctx, `
		INSERT INTO tokens (kind, token_hash, org_id, claims, max_uses)
		VALUES ('registration', $1, $2, $3::jsonb, 10)`,
		auth.Hash(token), orgID, claims); err != nil {
		t.Fatalf("store registration token: %v", err)
	}
	return token
}

func jsonStringArray(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// seedJob inserts a run and one job with the given steps, queued and ready.
func (h *harness) seedJob(t *testing.T, steps [][]string, opts ...func(*jobOptions)) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	o := jobOptions{executor: "shell", requiredTags: []string{}}
	for _, opt := range opts {
		opt(&o)
	}

	var orgID, repoID, runID uuid.UUID
	if err := h.store.Pool().QueryRow(ctx, `SELECT id FROM orgs LIMIT 1`).Scan(&orgID); err != nil {
		t.Fatalf("find org: %v", err)
	}
	if err := h.store.Pool().QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name) VALUES ($1, 'cicadoci', 'core') RETURNING id`,
		orgID).Scan(&repoID); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := h.store.Pool().QueryRow(ctx,
		`INSERT INTO runs (repo_id, number, event, ref, sha, actor, status)
		 VALUES ($1, 1, 'push', 'refs/heads/main', 'abc123', 'tester', 'running') RETURNING id`,
		repoID).Scan(&runID); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Insert the job as 'pending', attach its steps, and only then enqueue it.
	// A job must not become claimable until it is fully constructed: a
	// dispatcher woken by the doorbell will claim it within milliseconds, and
	// would otherwise run a job whose steps had not landed yet. This mirrors
	// what the scheduler does, which is why 'pending' is the schema default.
	var jobID uuid.UUID
	if err := h.store.Pool().QueryRow(ctx,
		`INSERT INTO jobs (run_id, name, status, executor, required_tags, env, timeout_seconds)
		 VALUES ($1, 'walking-skeleton', 'pending', $2, $3, '{"CICADO_JOB":"skeleton"}'::jsonb, 120)
		 RETURNING id`,
		runID, o.executor, o.requiredTags).Scan(&jobID); err != nil {
		t.Fatalf("create job: %v", err)
	}

	for i, command := range steps {
		if _, err := h.store.Pool().Exec(ctx,
			`INSERT INTO steps (job_id, step_index, name, command, env, timeout_seconds)
			 VALUES ($1, $2, $3, $4, '{}'::jsonb, 60)`,
			jobID, i, fmt.Sprintf("step-%d", i), command); err != nil {
			t.Fatalf("create step: %v", err)
		}
	}

	if err := h.queue.Enqueue(ctx, jobID); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	return jobID
}

type jobOptions struct {
	executor     string
	requiredTags []string
}

func withRequiredTags(tags ...string) func(*jobOptions) {
	return func(o *jobOptions) { o.requiredTags = tags }
}

// startRunner enrolls a runner against the harness and runs it until the
// context is cancelled.
func (h *harness) startRunner(t *testing.T, ctx context.Context, regToken string) string {
	t.Helper()

	conn, err := grpc.NewClient(h.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	enrollCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	resp, err := runner.Enroll(enrollCtx, conn, regToken, "test-runner", []string{"shell"})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	agent := runner.New(runner.Config{
		ServerAddress: h.addr,
		Credential:    resp.GetRunnerCredential(),
		RunnerID:      resp.GetRunnerId(),
		WorkspaceRoot: t.TempDir(),
		Capacity:      2,
		Executors:     []string{"shell"},
		Logger:        h.log,
	}, conn)

	go func() { _ = agent.Run(ctx) }()

	return resp.GetRunnerId()
}

// awaitJobStatus polls until a job reaches one of the wanted statuses.
func awaitJobStatus(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID, timeout time.Duration, want ...string) string {
	t.Helper()

	wanted := make(map[string]bool, len(want))
	for _, w := range want {
		wanted[w] = true
	}

	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(context.Background(),
			`SELECT status FROM jobs WHERE id = $1`, jobID).Scan(&last); err != nil {
			t.Fatalf("read job status: %v", err)
		}
		if wanted[last] {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach %v within %v (last status: %q)", jobID, want, timeout, last)
	return ""
}

// jobLogs returns everything the job's chunks printed, in sequence order.
func jobLogs(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) string {
	t.Helper()

	rows, err := pool.Query(context.Background(),
		`SELECT data FROM log_entries WHERE job_id = $1 AND kind = 'chunk' ORDER BY seq`, jobID)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	defer rows.Close()

	var sb strings.Builder
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			t.Fatalf("scan log chunk: %v", err)
		}
		sb.Write(data)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("log rows: %v", err)
	}
	return sb.String()
}

// TestWalkingSkeleton is the M0 exit criterion: a hardcoded job in the database
// runs on a runner over the real gRPC stream and reports back.
//
// The path exercised: seed job -> LISTEN/NOTIFY doorbell -> dispatcher claims
// with a lease -> JobAssignment down the Session stream -> JobAck up ->
// shell executor runs `echo` -> log chunks and structured events up the
// LogStream -> StatusUpdate(SUCCEEDED) up -> job marked succeeded.
func TestWalkingSkeleton(t *testing.T) {
	if testing.Short() {
		t.Skip("walking skeleton needs a database and spawns processes")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	h := startServer(t, ctx)
	regToken := h.mintRegistrationToken(t, []string{"linux", "shell"})
	h.startRunner(t, ctx, regToken)

	jobID := h.seedJob(t, [][]string{
		{"echo", "hello"},
		{"sh", "-c", "echo from cicado"},
	})

	status := awaitJobStatus(t, h.store.Pool(), jobID, 60*time.Second,
		"succeeded", "failed", "dead_letter")
	if status != "succeeded" {
		t.Fatalf("job finished %q, want succeeded. logs:\n%s", status, jobLogs(t, h.store.Pool(), jobID))
	}

	logs := jobLogs(t, h.store.Pool(), jobID)
	for _, want := range []string{"hello", "from cicado"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q; got:\n%s", want, logs)
		}
	}

	// The job must have gone through the real status lifecycle, not jumped
	// straight to a terminal state.
	var states []string
	rows, err := h.store.Pool().Query(ctx,
		`SELECT state FROM job_status_updates WHERE job_id = $1 ORDER BY seq`, jobID)
	if err != nil {
		t.Fatalf("read status updates: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan state: %v", err)
		}
		states = append(states, s)
	}
	if len(states) < 3 {
		t.Errorf("recorded states = %v, want at least preparing, running_step, succeeded", states)
	}
	if states[0] != "preparing" {
		t.Errorf("first state = %q, want preparing", states[0])
	}
	if states[len(states)-1] != "succeeded" {
		t.Errorf("last state = %q, want succeeded", states[len(states)-1])
	}

	// Structured events are the substrate the UI and MCP tools read, so the
	// skeleton proves they flow too, not just raw text.
	var eventKinds []string
	eventRows, err := h.store.Pool().Query(ctx,
		`SELECT event_kind FROM log_entries WHERE job_id = $1 AND kind = 'event' ORDER BY seq`, jobID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer eventRows.Close()
	for eventRows.Next() {
		var k string
		if err := eventRows.Scan(&k); err != nil {
			t.Fatalf("scan event kind: %v", err)
		}
		eventKinds = append(eventKinds, k)
	}
	if len(eventKinds) < 4 {
		t.Errorf("structured events = %v, want start/finish for both steps", eventKinds)
	}

	// The lease must be released on completion: a finished job holding a lease
	// would be invisible to the sweeper and un-rerunnable.
	var leaseID *uuid.UUID
	if err := h.store.Pool().QueryRow(ctx,
		`SELECT lease_id FROM jobs WHERE id = $1`, jobID).Scan(&leaseID); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if leaseID != nil {
		t.Error("a completed job still holds its lease")
	}
}

// A failing step must be reported as failed with its real exit code, not
// swallowed or reported as infrastructure trouble.
func TestFailingStepIsReportedWithExitCode(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a database and spawns processes")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	h := startServer(t, ctx)
	regToken := h.mintRegistrationToken(t, []string{"linux", "shell"})
	h.startRunner(t, ctx, regToken)

	jobID := h.seedJob(t, [][]string{
		{"sh", "-c", "echo about to fail; exit 17"},
		{"echo", "this step must never run"},
	})

	status := awaitJobStatus(t, h.store.Pool(), jobID, 60*time.Second,
		"succeeded", "failed", "dead_letter")
	if status != "failed" {
		t.Fatalf("job finished %q, want failed", status)
	}

	var exitCode int32
	var errorClass string
	if err := h.store.Pool().QueryRow(ctx,
		`SELECT exit_code, coalesce(error_class, '') FROM jobs WHERE id = $1`, jobID).
		Scan(&exitCode, &errorClass); err != nil {
		t.Fatalf("read job result: %v", err)
	}
	if exitCode != 17 {
		t.Errorf("exit code = %d, want 17", exitCode)
	}
	if errorClass != "user_script" {
		t.Errorf("error class = %q, want user_script", errorClass)
	}

	logs := jobLogs(t, h.store.Pool(), jobID)
	if !strings.Contains(logs, "about to fail") {
		t.Errorf("logs missing the failing step's output; got:\n%s", logs)
	}
	if strings.Contains(logs, "must never run") {
		t.Error("a step after a failing step ran; the job should have stopped")
	}
}

// Enrollment must take labels from the token, never from the request. A stolen
// token that could self-assign labels would become eligible for privileged jobs.
func TestRunnerCannotSelfAssignLabels(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	h := startServer(t, ctx)
	// The token grants only the unprivileged label.
	regToken := h.mintRegistrationToken(t, []string{"linux"})

	conn, err := grpc.NewClient(h.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	client := runnerv1.NewRunnerServiceClient(conn)
	resp, err := client.Register(ctx, &runnerv1.RegisterRequest{
		RegistrationToken: regToken,
		Name:              "greedy-runner",
		Capabilities: &runnerv1.Capabilities{
			Executors: []string{"shell"},
			Os:        "linux",
			Arch:      "amd64",
			// Asking for a label the token was not minted with.
			Features: []string{"deploy-prod"},
		},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	for _, label := range resp.GetLabels() {
		if label == "deploy-prod" {
			t.Fatal("runner was granted a privileged label it asked for; labels must come from the token")
		}
	}
	if len(resp.GetLabels()) != 1 || resp.GetLabels()[0] != "linux" {
		t.Errorf("labels = %v, want exactly the token's [linux]", resp.GetLabels())
	}

	// And the label must actually gate scheduling, not just be cosmetic.
	h.startRunner(t, ctx, h.mintRegistrationTokenForExistingOrg(t, []string{"linux"}))
	jobID := h.seedJob(t, [][]string{{"echo", "privileged"}}, withRequiredTags("deploy-prod"))

	time.Sleep(3 * time.Second)
	var status string
	if err := h.store.Pool().QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, jobID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "queued" {
		t.Errorf("job requiring deploy-prod reached status %q; no runner should be eligible", status)
	}
}

// An invalid registration token must be refused.
func TestRegistrationRejectsBadToken(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	h := startServer(t, ctx)

	conn, err := grpc.NewClient(h.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	client := runnerv1.NewRunnerServiceClient(conn)
	for _, badToken := range []string{
		"",
		"not-a-token",
		"cicado_reg_" + strings.Repeat("A", 43), // well-formed but never issued
	} {
		if _, err := client.Register(ctx, &runnerv1.RegisterRequest{
			RegistrationToken: badToken,
			Name:              "intruder",
			Capabilities:      &runnerv1.Capabilities{Executors: []string{"shell"}},
		}); err == nil {
			t.Errorf("Register accepted token %q", badToken)
		}
	}
}

// The protocol requires Hello first; anything else must close the stream.
func TestSessionRequiresHelloFirst(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	h := startServer(t, ctx)
	regToken := h.mintRegistrationToken(t, []string{"linux"})

	conn, err := grpc.NewClient(h.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	resp, err := runner.Enroll(ctx, conn, regToken, "protocol-test", []string{"shell"})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	authCtx := bearerContext(ctx, resp.GetRunnerCredential())
	client := runnerv1.NewRunnerServiceClient(conn)
	stream, err := client.Session(authCtx)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}

	// Send a heartbeat as the first message, which the protocol forbids.
	if err := stream.Send(&runnerv1.RunnerMessage{
		Msg: &runnerv1.RunnerMessage_Heartbeat{Heartbeat: &runnerv1.Heartbeat{}},
	}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("server accepted a session whose first message was not Hello")
	}
}

// An unauthenticated session must be refused outright.
func TestSessionRequiresCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	h := startServer(t, ctx)

	conn, err := grpc.NewClient(h.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	client := runnerv1.NewRunnerServiceClient(conn)
	stream, err := client.Session(ctx)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if err := stream.Send(&runnerv1.RunnerMessage{
		Msg: &runnerv1.RunnerMessage_Hello{Hello: &runnerv1.Hello{ProtocolVersion: 1}},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("server accepted a session with no credentials")
	}
}

// mintRegistrationTokenForExistingOrg mints another token against the org that
// already exists, for tests that enroll more than one runner.
func (h *harness) mintRegistrationTokenForExistingOrg(t *testing.T, labels []string) string {
	t.Helper()
	ctx := context.Background()

	var orgID uuid.UUID
	if err := h.store.Pool().QueryRow(ctx, `SELECT id FROM orgs LIMIT 1`).Scan(&orgID); err != nil {
		t.Fatalf("find org: %v", err)
	}

	token, err := auth.Generate(auth.KindRegistration)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	claims := fmt.Sprintf(`{"labels": %s}`, jsonStringArray(labels))
	if _, err := h.store.Pool().Exec(ctx, `
		INSERT INTO tokens (kind, token_hash, org_id, claims, max_uses)
		VALUES ('registration', $1, $2, $3::jsonb, 10)`,
		auth.Hash(token), orgID, claims); err != nil {
		t.Fatalf("store registration token: %v", err)
	}
	return token
}

// bearerContext attaches a runner credential to outgoing gRPC calls.
func bearerContext(ctx context.Context, credential string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+credential)
}

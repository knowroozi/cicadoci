// Package runnersvc implements the gRPC RunnerService: the server side of the
// frozen runner protocol (ADR-004, project-docs/04-runner-protocol.md).
//
// The protocol's shape is load-bearing and worth restating: runners dial
// outbound and the server never connects back, so everything the server wants a
// runner to do travels down a stream the runner opened. Control (Session) and
// bulk logs (LogStream) are separate streams on that one connection, so a job
// producing megabytes of output cannot delay a cancellation.
package runnersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
	"cicadoci.com/cicado/server/auth"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/redact"
	"cicadoci.com/cicado/server/secrets"
)

// Config tunes the server side of the protocol. The values here are what
// ServerHello advertises; runners take them as authoritative.
type Config struct {
	// HeartbeatInterval is how often a runner must heartbeat. The lease TTL is
	// 3x this with a 60s floor, so one dropped heartbeat never costs a runner
	// its jobs.
	HeartbeatInterval time.Duration
	MaxLogChunkBytes  uint32
	MaxInflightChunks uint32
	// AckInterval is how often the LogStream handler advances the watermark and
	// emits a LogAck. Zero uses DefaultLogStreamConfig().AckInterval (200ms,
	// CIC-30).
	AckInterval      time.Duration
	JournalDiskBytes uint64
	// MinSupportedRunner and LatestRunner are advertised so runners can warn or
	// self-update. Enforcement happens at Register, not here.
	MinSupportedRunner string
	LatestRunner       string
	// Masker scrubs registered secret values out of log payload
	// bytes before they are persisted to log_chunks or
	// structured_events. DefaultConfig() returns a
	// *RegisteredMasker (empty, no registered values) so that
	// server-side initialisation can register newly-minted
	// tokens without changing the default type. Zero-value
	// is treated as NoopMasker (no scrubbing, no panic).
	//
	// GetJobSecrets calls RegisterWithVariants on the masker for each
	// delivered secret so that secret values that appear in log
	// payloads are scrubbed before persistence or fan-out (CIC-74).
	Masker redact.Masker
	// LogPublish is called after a chunk or event is persisted, before the
	// watermark is advanced. It is the fan-out hook for the SSE hub
	// (CIC-33). data is exactly what was persisted, so it has already been
	// through Masker (CIC-82): live viewers never see a registered secret.
	// When nil, log emission is a no-op.
	LogPublish func(jobID uuid.UUID, kind string, seq int64, data []byte)
	// OnRunTerminal is called exactly once per run, after the transaction
	// that moved the run's last job to a terminal state has committed. state
	// is the run's final status: "succeeded", "failed" or "cancelled". New
	// installs it on the queue, which is where every terminal job transition
	// happens (runner report, sweeper dead letter, cancel scan), so it fires
	// for all of them. It runs on the caller's goroutine and must not block.
	//
	// This is the single run-level hook: forge check completion and the
	// backpressure release attach here. When nil, runs are still finalised
	// in the database; nothing else is notified.
	OnRunTerminal func(runID uuid.UUID, state string)
	// CloneMinter mints a per-job GitHub installation token on demand.
	// When non-nil, GetJobSecrets calls it for secret_refs that include
	// "github_clone_token" instead of looking the value up from the
	// static secrets store (CIC-151 / CIC-146). When nil, GetJobSecrets
	// treats that secret like any other — looked up from the store.
	CloneMinter CloneMinterFunc
}

// DefaultConfig returns workable defaults for a development install.
func DefaultConfig() Config {
	return Config{
		HeartbeatInterval:  20 * time.Second,
		MaxLogChunkBytes:   64 * 1024,
		MaxInflightChunks:  256,
		AckInterval:        200 * time.Millisecond,
		JournalDiskBytes:   512 * 1024 * 1024,
		MinSupportedRunner: "0.1.0",
		LatestRunner:       "0.1.0",
		Masker:             redact.NewRegisteredMasker(),
	}
}

// LeaseTTL derives the lease lifetime from the heartbeat interval.
func (c Config) LeaseTTL() time.Duration {
	ttl := 3 * c.HeartbeatInterval
	if ttl < 60*time.Second {
		ttl = 60 * time.Second
	}
	return ttl
}

// Service implements runnerv1.RunnerServiceServer.
type Service struct {
	runnerv1.UnimplementedRunnerServiceServer

	pool     *pgxpool.Pool
	queue    *queue.Postgres
	cfg      Config
	log      *slog.Logger
	sessions *SessionRegistry
	// secrets is nil when secret delivery is not configured.
	// A nil store means GetJobSecrets returns an empty response.
	secrets secrets.Store
}

// New builds the service.
//
// A nil Masker is treated as "no scrubbing yet" and replaced with
// NoopMasker, so the log ingest path never sees a nil interface.
// secretsStore may be nil for a dev server that does not yet
// deliver secret material; it is set in main.go once the
// Postgres-backed secrets store is constructed.
func New(pool *pgxpool.Pool, q *queue.Postgres, cfg Config, log *slog.Logger, secretsStore secrets.Store) *Service {
	if cfg.Masker == nil {
		cfg.Masker = redact.NoopMasker{}
	}
	if cfg.OnRunTerminal != nil {
		q.SetRunTerminalHook(cfg.OnRunTerminal)
	}
	return &Service{
		pool:     pool,
		queue:    q,
		cfg:      cfg,
		log:      log,
		sessions: NewSessionRegistry(),
		secrets:  secretsStore,
	}
}

// Sessions exposes the registry so the dispatcher can find connected runners.
func (s *Service) Sessions() *SessionRegistry { return s.sessions }

// Register enrolls a runner using an operator-minted registration token.
//
// Labels come from the TOKEN, never from the request. A stolen registration
// token can enroll a runner, which is bad enough; it must not additionally let
// the thief claim a label like `deploy-prod` that the token was not minted with
// and thereby become eligible for privileged jobs.
func (s *Service) Register(ctx context.Context, req *runnerv1.RegisterRequest) (*runnerv1.RegisterResponse, error) {
	if err := auth.Validate(auth.KindRegistration, req.GetRegistrationToken()); err != nil {
		// Deliberately the same error as an unknown token: a caller must not be
		// able to distinguish "malformed" from "not recognized".
		return nil, status.Error(codes.PermissionDenied, "invalid registration token")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, "begin transaction")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the token row: two runners enrolling concurrently with the same
	// limited-use token must not both slip past the max_uses check.
	var (
		tokenID uuid.UUID
		orgID   uuid.UUID
		labels  []string
		uses    int32
		maxUses int32
	)
	err = tx.QueryRow(ctx, `
		SELECT t.id, t.org_id,
		       coalesce(array(SELECT jsonb_array_elements_text(t.claims->'labels')), '{}'),
		       t.uses, t.max_uses
		  FROM tokens t
		 WHERE t.token_hash = $1
		   AND t.kind = 'registration'
		   AND t.revoked_at IS NULL
		   AND (t.expires_at IS NULL OR t.expires_at > now())
		   FOR UPDATE`,
		auth.Hash(req.GetRegistrationToken())).Scan(&tokenID, &orgID, &labels, &uses, &maxUses)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.PermissionDenied, "invalid registration token")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "look up registration token")
	}
	if uses >= maxUses {
		return nil, status.Error(codes.PermissionDenied, "registration token is exhausted")
	}

	caps := req.GetCapabilities()
	name := req.GetName()
	if name == "" {
		name = "runner-" + uuid.NewString()[:8]
	}

	var runnerID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO runners (org_id, name, labels, executors, os, arch, runner_version, features, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'offline')
		RETURNING id`,
		orgID, name, nonNil(labels), nonNil(caps.GetExecutors()), caps.GetOs(), caps.GetArch(),
		caps.GetRunnerVersion(), nonNil(caps.GetFeatures())).Scan(&runnerID); err != nil {
		return nil, status.Error(codes.Internal, "create runner")
	}

	credential, err := auth.Generate(auth.KindRunner)
	if err != nil {
		return nil, status.Error(codes.Internal, "mint credential")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tokens (kind, token_hash, org_id, runner_id, claims)
		VALUES ('runner', $1, $2, $3, '{}'::jsonb)`,
		auth.Hash(credential), orgID, runnerID); err != nil {
		return nil, status.Error(codes.Internal, "store credential")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tokens SET uses = uses + 1, last_used_at = now() WHERE id = $1`, tokenID); err != nil {
		return nil, status.Error(codes.Internal, "record token use")
	}

	if err := s.audit(ctx, tx, auditEvent{
		OrgID:         &orgID,
		PrincipalType: "runner",
		PrincipalID:   runnerID.String(),
		Surface:       "api",
		Action:        "runner.registered",
		SubjectType:   "runner",
		SubjectID:     runnerID.String(),
	}); err != nil {
		return nil, status.Error(codes.Internal, "write audit event")
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, status.Error(codes.Internal, "commit registration")
	}

	s.log.Info("runner registered",
		"runner_id", runnerID, "name", name, "labels", labels,
		"version", caps.GetRunnerVersion())

	return &runnerv1.RegisterResponse{
		RunnerId:         runnerID.String(),
		RunnerCredential: credential,
		Labels:           labels,
	}, nil
}

// GetJobSecrets returns secret material for a job, authorized against the live
// lease.
//
// This is the only path by which a secret value leaves the control plane. The
// lease check is the authorization: a runner that no longer holds the job's
// live lease gets nothing, even with a valid credential, because the job may
// have been reassigned to someone else while it was out of contact.
//
// Security invariants enforced here:
//   - Fork PR runs receive no secrets (run.is_fork check).
//   - Secret values are registered in the server-side redaction masker
//     immediately before being returned, so a subsequent log payload that
//     leaks a value is scrubbed before persistence or fan-out.
//   - A missing or unknown secret_ref produces a log warning, not a failure:
//     the job can proceed with the secrets it was configured for.
func (s *Service) GetJobSecrets(ctx context.Context, req *runnerv1.GetJobSecretsRequest) (*runnerv1.GetJobSecretsResponse, error) {
	runnerID, err := s.authenticate(ctx)
	if err != nil {
		return nil, err
	}

	jobID, err := uuid.Parse(req.GetJobId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed job id")
	}
	leaseID, err := uuid.Parse(req.GetLeaseId())
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "lease is not valid for this job")
	}

	// Verify the lease: the job must be live, claimed by this runner, and
	// the lease ID must match the live lease (prevents replay with a stale
	// lease after a requeue).
	var (
		secretRefs []string
		runID      uuid.UUID
	)
	err = s.pool.QueryRow(ctx, `
		SELECT coalesce(job.secret_refs, '{}'), job.run_id
		  FROM jobs job
		 WHERE job.id = $1
		   AND job.lease_id = $2
		   AND job.claimed_by = $3
		   AND job.status IN ('claimed', 'running')`,
		jobID, leaseID, runnerID).Scan(&secretRefs, &runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.PermissionDenied, "lease is not valid for this job")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "look up job")
	}

	// S3: the clone minter can mint even when s.secrets is nil (CIC-151);
	// so skip only when BOTH store and minter are nil.
	if s.secrets == nil && s.cfg.CloneMinter == nil {
		return &runnerv1.GetJobSecretsResponse{}, nil
	}

	// Fetch run metadata: is_fork and repo_id.
	var (
		isFork bool
		repoID uuid.UUID
	)
	err = s.pool.QueryRow(ctx,
		`SELECT is_fork, repo_id FROM runs WHERE id = $1`, runID).Scan(&isFork, &repoID)
	if err != nil {
		return nil, status.Error(codes.Internal, "look up run")
	}
	// B1: fork PR runs get ONLY github_clone_token via the clone minter
	// (CIC-151 / conv-e2490b #2); all other secret refs are refused
	// for fork runs (CIC-27 defense-in-depth). When no minter is
	// configured a fork run gets nothing.
	if isFork && s.cfg.CloneMinter == nil {
		s.log.Warn("GetJobSecrets: fork run, no clone minter configured, refusing all secrets",
			"run_id", runID.String())
		return &runnerv1.GetJobSecretsResponse{}, nil
	}
	// Look up org_id from the repo row.
	var orgID uuid.UUID
	err = s.pool.QueryRow(ctx,
		`SELECT org_id FROM repos WHERE id = $1`, repoID).Scan(&orgID)
	if err != nil {
		return nil, status.Error(codes.Internal, "look up repo")
	}

	resp := &runnerv1.GetJobSecretsResponse{Secrets: make(map[string]string, len(secretRefs))}

	for _, name := range secretRefs {
		// CIC-151 / CIC-146: dynamically mint a per-job GitHub
		// installation token instead of looking it up from the
		// store. The token is NEVER persisted. Fork PR runs get
		// a contents:read clone token; all other secret refs are
		// refused for fork runs (CIC-27).
		if name == "github_clone_token" && s.cfg.CloneMinter != nil {
			token, err := s.cfg.CloneMinter(ctx,
				orgID.String(), repoID.String(), isFork)
			if err != nil {
				// B3c: log as error, not warning. The token is
				// omitted so the job's step fails with
				// secret_unavailable; the error is logged
				// without any key material.
				s.log.Error("GetJobSecrets: clone minter failed; github_clone_token omitted, job will fail",
					"secret", name, "run_id", runID.String(),
					"org_id", orgID.String(), "repo_id", repoID.String(),
					"err", err)
				return nil, status.Error(codes.Internal, "clone minter failed")
			}
			// B5 soft-skip: minter returned ("", nil) when
			// installation_id is not yet set (pre-CIC-146).
			// The step gets an anonymous clone; omit the secret
			// ref rather than sending an empty value.
			if token == "" {
				continue
			}
			if rm, ok := s.cfg.Masker.(redact.RegisteringMasker); ok {
				// The minter already registered with its own Masker
				// (CIC-74); registering again guarantees the
				// server-side log masker knows the token value
				// before it leaves the server process.
				rm.RegisterWithVariants(token)
			}
			resp.Secrets[name] = token
			continue
		}
		if isFork {
			s.log.Warn("GetJobSecrets: fork run cannot access static secret, skipping",
				"secret", name, "run_id", runID.String())
			continue
		}
		// S3: s.secrets must be non-nil for static-store lookups.
		if s.secrets == nil {
			return nil, status.Error(codes.Internal, "secrets store not configured for GetJobSecrets")
		}
		lease, err := s.secrets.Get(ctx, orgID.String(), repoID.String(), name)
		if err != nil {
			// A missing secret is a configuration error, not a security
			// failure. Log it and move on: the job can run with the
			// secrets it is actually configured for.
			s.log.Warn("GetJobSecrets: secret not found, skipping",
				"secret", name, "run_id", runID.String(), "org_id", orgID.String(), "repo_id", repoID.String())
			continue
		}
		defer lease.Close() // zero the value before we return

		// Register with the server-side masker so that a subsequent log
		// payload containing this value is scrubbed before persistence
		// or fan-out (CIC-74 invariant).
		if rm, ok := s.cfg.Masker.(redact.RegisteringMasker); ok {
			rm.RegisterWithVariants(string(lease.Value))
		}

		resp.Secrets[name] = string(lease.Value)
	}

	return resp, nil
}

// authenticate resolves the runner credential on the request into a runner id.
func (s *Service) authenticate(ctx context.Context) (uuid.UUID, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return uuid.Nil, status.Error(codes.Unauthenticated, "missing credentials")
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		return uuid.Nil, status.Error(codes.Unauthenticated, "missing credentials")
	}
	token, ok := bearer(values[0])
	if !ok {
		return uuid.Nil, status.Error(codes.Unauthenticated, "malformed credentials")
	}
	if err := auth.Validate(auth.KindRunner, token); err != nil {
		return uuid.Nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}

	var runnerID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT runner_id FROM tokens
		 WHERE token_hash = $1 AND kind = 'runner' AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())`,
		auth.Hash(token)).Scan(&runnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	if err != nil {
		return uuid.Nil, status.Error(codes.Internal, "verify credentials")
	}
	return runnerID, nil
}

func bearer(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) {
		return "", false
	}
	// Case-insensitive scheme, per RFC 7235.
	if !equalFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return header[len(prefix):], true
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// RotateCredential issues a new runner credential. The old one stays valid
// until the new one is first used, so a runner that crashes mid-rotation can
// still get back in with what it has on disk.
func (s *Service) RotateCredential(ctx context.Context, _ *runnerv1.RotateCredentialRequest) (*runnerv1.RotateCredentialResponse, error) {
	return nil, status.Error(codes.Unimplemented, "credential rotation lands with the token subsystem (CIC-40)")
}

// nonNil turns a nil slice into an empty one. pgx encodes a nil []string as SQL
// NULL, which the schema's NOT NULL array columns reject; an absent repeated
// proto field arrives as nil, not as an empty slice.
func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

type auditEvent struct {
	OrgID         *uuid.UUID
	PrincipalType string
	PrincipalID   string
	Surface       string
	Action        string
	SubjectType   string
	SubjectID     string
}

// audit writes an audit event inside the caller's transaction, so an action and
// its audit record commit together or not at all.
func (s *Service) audit(ctx context.Context, tx pgx.Tx, e auditEvent) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO audit_events
			(org_id, principal_type, principal_id, surface, action, subject_type, subject_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		e.OrgID, e.PrincipalType, e.PrincipalID, e.Surface, e.Action, e.SubjectType, e.SubjectID)
	if err != nil {
		return fmt.Errorf("runnersvc: write audit event: %w", err)
	}
	return nil
}

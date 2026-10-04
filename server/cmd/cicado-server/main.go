// Command cicado-server is the CICaDo control plane.
//
// One binary: HTTP API, gRPC runner service, scheduler, and (from M6) the web
// UI embedded via embed.FS. Its only stateful dependency is Postgres.
//
// CIC-71d wires the controller (CIC-71a/b/c) into the webhook receiver.
// The controller is used as the webhook.Dispatcher for the receiver.
// CIC-81: with --github-app-token set, the GitHub forge reports each run as
// a check on the commit; without it, a NoOp forge reports nothing.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
	"cicadoci.com/cicado/server/api"
	"cicadoci.com/cicado/server/archive"
	"cicadoci.com/cicado/server/audit"
	"cicadoci.com/cicado/server/blobstore"
	"cicadoci.com/cicado/server/controller"
	"cicadoci.com/cicado/server/dispatch"
	"cicadoci.com/cicado/server/forge"
	githubpkg "cicadoci.com/cicado/server/github"
	"cicadoci.com/cicado/server/logs"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/runnersvc"
	"cicadoci.com/cicado/server/secrets"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/webhook"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "cicado-server: %v\n", err)
		os.Exit(1)
	}
}

// serverFlags holds the command-line flag pointers for cicado-server. The
// registrations live in registerFlags so the flag set is defined once and the
// B1-test can exercise it on a fresh FlagSet (verifying, for example, that the
// CIC-150 API-token flag carries no non-empty default, which PrintDefaults
// would otherwise print into the usage text).
type serverFlags struct {
	grpcAddr             *string
	httpAddr             *string
	databaseURL          *string
	migrate              *bool
	devSeedFlag          *bool
	logLevel             *string
	webhookSecret        *string
	insecureWebhook      *bool
	webhookMaxConcurrent *int
	secretKeyHex         *string
	allowEphemeralKey    *bool
	githubWorkflowToken  *string
	githubAppToken       *string
	apiToken             *string
	insecureAPI          *bool
}

// registerFlags registers all of cicado-server's command-line flags on fs and
// returns the pointers. Both run() (on flag.CommandLine) and the tests (on a
// fresh flag.FlagSet) call it, so the registered flags are always the same.
// The apiToken flag keeps an empty default (not os.Getenv(...)) so its value
// is never printed in the flag usage text; run() reads CICADO_API_TOKEN after
// flag.Parse().
func registerFlags(fs *flag.FlagSet) *serverFlags {
	return &serverFlags{
		grpcAddr:             fs.String("grpc-addr", envOr("CICADO_GRPC_ADDR", ":8081"), "address for the gRPC runner service"),
		httpAddr:             fs.String("http-addr", envOr("CICADO_HTTP_ADDR", ":8080"), "address for the HTTP API"),
		databaseURL:          fs.String("database-url", "", "Postgres connection string; read from CICADO_DATABASE_URL after flag.Parse() so a password-bearing DSN never appears in the flag usage text (CIC-150 B4 / CIC-159)"),
		migrate:              fs.Bool("migrate", envBool("CICADO_MIGRATE", true), "apply pending migrations at startup"),
		devSeedFlag:          fs.Bool("dev-seed", envBool("CICADO_DEV_SEED", false), "seed a development org, a well-known registration token, and a demo job (NEVER in production)"),
		logLevel:             fs.String("log-level", envOr("CICADO_LOG_LEVEL", "info"), "debug, info, warn, or error"),
		webhookSecret:        fs.String("webhook-secret", "", "GitHub App webhook signing secret; empty disables verification unless --insecure-webhook is also set. Read from CICADO_WEBHOOK_SECRET after flag.Parse() so the secret never appears in the flag usage text (CIC-150 B4 / CIC-159)"),
		insecureWebhook:      fs.Bool("insecure-webhook", envBool("CICADO_INSECURE_WEBHOOK", false), "permit running with no webhook signing secret (fail-open); DEVELOPMENT ONLY — GitHub deliveries accepted without HMAC verification"),
		webhookMaxConcurrent: fs.Int("webhook-max-concurrent", 16, "max concurrent webhook dispatches (flood backpressure); 0 disables"),
		secretKeyHex:         fs.String("secret-key", "", "hex-encoded 32-byte AES-256 encryption key for the secrets store. Read from CICADO_SECRET_KEY after flag.Parse() so the key never appears in the flag usage text (CIC-150 B4 / CIC-159)"),
		allowEphemeralKey:    fs.Bool("allow-ephemeral-key", envBool("CICADO_ALLOW_EPHEMERAL_KEY", false), "permit an ephemeral random secret key when CICADO_SECRET_KEY is empty (DEVELOPMENT ONLY: secrets will not survive a restart)"),
		githubWorkflowToken:  fs.String("github-workflow-token", "", "long-lived fine-grained GitHub personal access token (Contents: read) used to fetch the repo's workflow files on a default-branch push; NOT a GitHub App installation token, which expires after 1 h. Read from CICADO_GITHUB_WORKFLOW_TOKEN after flag.Parse() so the token never appears in the flag usage text (a flag value shows in ps); empty keeps the producer off and relies on the store/dev-seed path (CIC-150 B4 / CIC-159)"),
		githubAppToken:       fs.String("github-app-token", "", "GitHub token used to report runs as checks (Checks API); empty reports nothing. A GitHub App installation token expires after 1 hour, so a static one suits short-lived dev servers only; minting per call is CIC-146. The Checks API accepts only App installation tokens, not PATs. Read from CICADO_GITHUB_APP_TOKEN after flag.Parse() so the token never appears in the flag usage text (CIC-150 B4 / CIC-159)"),
		// CIC-150: empty (not os.Getenv(...)) default so the secret's value is
		// never printed in the flag usage text; read from CICADO_API_TOKEN after
		// flag.Parse() in run().
		apiToken:    fs.String("api-token", "", "bearer secret required on every /api/v1/* request except webhooks and health probes (CIC-150); empty refuses to start unless --insecure-api is also set. Supplied via the CICADO_API_TOKEN env var, read after flag.Parse() so the secret never appears in the flag usage text"),
		insecureAPI: fs.Bool("insecure-api", envBool("CICADO_INSECURE_API", false), "permit /api/v1/* without an API token (fail-open; DEVELOPMENT ONLY -- CIC-150)"),
	}
}

func run() error {
	// Registration lives in registerFlags(fs) so the flag set is registered
	// once and the B1-test can exercise it on a fresh FlagSet; run() uses the
	// returned pointers under the same bare names as before.
	f := registerFlags(flag.CommandLine)
	grpcAddr, httpAddr, databaseURL, migrate, devSeedFlag, logLevel, webhookSecret,
		insecureWebhook, webhookMaxConcurrent, secretKeyHex, allowEphemeralKey,
		githubWorkflowToken, githubAppToken, apiToken, insecureAPI :=
		f.grpcAddr, f.httpAddr, f.databaseURL, f.migrate, f.devSeedFlag, f.logLevel,
		f.webhookSecret, f.insecureWebhook, f.webhookMaxConcurrent, f.secretKeyHex,
		f.allowEphemeralKey, f.githubWorkflowToken, f.githubAppToken, f.apiToken, f.insecureAPI

	flag.Parse()

	// CIC-150 B1/B4 / CIC-159 B1: resolve all six secret / token flags through the
	// SAME helper the tests call (resolveSecretFlags below, which funnels through
	// envSecret). run() calls this once so dropping the call from run() is caught
	// by TestDBEnvFallbackIsWiredIntoRun; dropping any single assignment in
	// resolveSecretFlags is caught by TestResolveSecretFlags. No behaviour change
	// for a non-empty env value: it was read at registration time before this
	// change.
	resolveSecretFlags(f)

	log := newLogger(*logLevel)
	slog.SetDefault(log)

	// Fail closed on a missing webhook signing secret unless the operator
	// explicitly opted in to the development-only fail-open path.  This is
	// the operational half of CIC-80: a production server with no webhook
	// secret must not start and accept unverified deliveries.
	if err := webhookStartupGuard(*webhookSecret, *insecureWebhook); err != nil {
		return err
	}

	// CIC-150: fail closed on a missing API token unless the operator
	// explicitly opted in to the development-only fail-open path, mirroring
	// the webhook guard above.
	if err := apiStartupGuard(*apiToken, *insecureAPI); err != nil {
		return err
	}

	if *databaseURL == "" {
		return errors.New("no database configured: set CICADO_DATABASE_URL or pass -database-url")
	}

	// Signal handling before anything else, so a Ctrl-C during startup works.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, *databaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	if *migrate {
		log.Info("applying migrations")
		if err := st.Migrate(ctx); err != nil {
			return err
		}
		version, err := st.Version(ctx)
		if err != nil {
			return err
		}
		log.Info("schema ready", "version", version)
	}

	// Construct the secrets store.
	//
	// A persistent key (CICADO_SECRET_KEY) is required in production.
	// Without it the server refuses to start unless --allow-ephemeral-key
	// was also passed, preventing silent loss of encrypted-at-rest data.
	var secretsStore secrets.Store
	if *secretKeyHex != "" {
		keyBytes, err := hex.DecodeString(*secretKeyHex)
		if err != nil {
			return fmt.Errorf("invalid secret key (must be hex): %w", err)
		}
		if len(keyBytes) != 32 {
			return fmt.Errorf("secret key must be 32 bytes, got %d", len(keyBytes))
		}
		var key secrets.EncryptionKey
		copy(key[:], keyBytes)
		secretsStore = secrets.NewPostgres(st, key)
		log.Info("secrets store active", "key_source", "CICADO_SECRET_KEY")
	} else if *devSeedFlag || *allowEphemeralKey {
		// Ephemeral random key: secrets will not survive a restart.
		key, err := secrets.GenerateKey()
		if err != nil {
			return fmt.Errorf("generate ephemeral key: %w", err)
		}
		secretsStore = secrets.NewPostgres(st, key)
		log.Warn("using an ephemeral key — secrets will be unreadable after a restart; set CICADO_SECRET_KEY for production",
			"ephemeral", true)
	} else {
		return errors.New(
			"CICADO_SECRET_KEY is not set and --allow-ephemeral-key was not passed; " +
				"set CICADO_SECRET_KEY for a persistent key or pass --allow-ephemeral-key")
	}

	q := queue.NewPostgres(st.Pool())

	if *devSeedFlag {
		if err := devSeed(ctx, st, q, log); err != nil {
			return err
		}
	}

	// CIC-82: per-job live logs over SSE. The runner service publishes each
	// persisted (already masked) chunk or event to the hub; the SSE handler
	// replays history through the compactor and then tails the hub. The blob
	// store is in-memory until an S3 store is configured, so archived logs do
	// not survive a restart.
	logHub := logs.NewHub(1024)
	sse := logs.NewSSE(logHub, archive.NewCompactor(st.Pool(), blobstore.NewMemory()))

	svcCfg := runnersvc.DefaultConfig()
	svcCfg.LogPublish = func(jobID uuid.UUID, kind string, seq int64, data []byte) {
		logHub.Publish(jobID, archive.Entry{Seq: seq, Kind: kind, Data: data})
	}
	// CIC-103: the one run-terminal hook. The queue fires it exactly once per
	// run, after the transaction that finished the run's last job commits
	// (runner report, sweeper dead letter, or cancel scan alike). It must be
	// set here, before runnersvc.New copies svcCfg. forge.Complete and the
	// backpressure release attach to this hook rather than adding their own.
	//
	// CIC-81: it completes the run's forge check. ctrl is built further down;
	// the hook only fires once the servers below are running, by which time
	// ctrl is set. The queue calls the hook on the goroutine that finished the
	// run and it must not block, so the forge round trip runs on its own
	// goroutine, bounded by a timeout.
	//
	// CIC-106: it also returns the run's backpressure slot, which the
	// controller holds from the webhook until the run is terminal. That is a
	// map update under a mutex, so it runs inline.
	var ctrl *controller.Controller
	svcCfg.OnRunTerminal = func(runID uuid.UUID, state string) {
		log.Info("run finished", "run_id", runID, "state", state)
		ctrl.ReleaseRun(runID)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := ctrl.RunCompleted(ctx, runID, state); err != nil {
				log.Warn("forge check completion failed", "run_id", runID, "state", state, "error", err)
			}
		}()
	}
	svc := runnersvc.New(st.Pool(), q, svcCfg, log, secretsStore)
	// The dispatcher gets no backpressure tracker (CIC-106). Run-level
	// backpressure is the controller's: it holds one slot per run until the
	// run is terminal. A dispatcher tracker would Acquire per job and never
	// release on completion, stalling job 5+ of every run.
	dispatcher := dispatch.New(q, svc.Sessions(), dispatch.DefaultConfig(svcCfg.LeaseTTL()), log)

	// CIC-71d: wire the controller as the webhook dispatcher.
	//
	// CIC-81: newForge picks the GitHub Checks forge when a token is
	// configured and the NoOp forge otherwise. The controller logs a
	// warning if Prepare/Complete fails; a run never fails on account of
	// its check.
	// Wire a persistent audit log so the "every state-changing
	// action writes an audit event" invariant produces a durable, read-later
	// trail rather than process memory that vanishes on restart.
	auditLog := audit.NewPostgresLog(st.Pool())
	ctrlOpts := controller.DefaultConfig().WithAudit(auditLog)
	// Wire the store-backed workflow loader so runs produce job DAGs.
	// With no loader, LoadAndCreateJobs is a silent no-op and the server accepts
	// webhooks but never creates jobs (the finding CIC-78 closed). StoreLoader
	// reads the workflows table; the production producer that fills the table is
	// the GitHub-raw fetch, tracked as its own follow-up producer ticket (NOT
	// CIC-38/39/40, which are the secrets RPC / OIDC issuer / job tokens), or a
	// dev-seed / CLI ingest in dev. A nil table simply means no jobs, which is
	// the safe default.
	ctrlOpts.WorkflowLoader = controller.NewStoreLoader(st, log)

	// CIC-120: wire the production workflow producer. When a token is
	// configured, a push to the default branch syncs the repo's workflow files
	// from the code host into the workflows table (upsert present files, delete
	// removed ones) *before* the loader reads them. The #95 gates live in the
	// controller (push on the default branch only; PRs never invoke the
	// producer, so no fork-supplied definition reaches the shared repo row). An
	// empty token leaves the producer nil and keeps the store/dev-seed path.
	//
	// TODO(CIC-146): the token is one static, long-lived fine-grained PAT.
	// Mint GitHub App installation tokens per call and decode installation.id
	// from the webhook instead.
	if p := newWorkflowProducer(*githubWorkflowToken, st, log, &http.Client{Timeout: 20 * time.Second}); p != nil {
		ctrlOpts.Producer = p
		log.Info("workflow producer active",
			"source", "github", "token_source", "CICADO_GITHUB_WORKFLOW_TOKEN")
	}

	ctrl = controller.New(st, newForge(*githubAppToken, log), ctrlOpts, log)

	grpcServer := grpc.NewServer()
	runnerv1.RegisterRunnerServiceServer(grpcServer, svc)

	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthSrv)
	// Reflection makes grpcurl work against a dev server, which is worth a lot
	// when debugging the protocol by hand.
	reflection.Register(grpcServer)

	var listenCfg net.ListenConfig
	listener, err := listenCfg.Listen(ctx, "tcp", *grpcAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *grpcAddr, err)
	}

	webhookReceiver := webhook.NewReceiver(webhook.ReceiverOptions{
		Secret:               *webhookSecret,
		MaxConcurrent:        *webhookMaxConcurrent,
		Dispatch:             ctrl,
		AllowUnauthenticated: *insecureWebhook,
	})
	// CIC-150 B4: arm the fail-closed API gate only when a real token is present.
	// An empty/absent token is the explicit --insecure-api development path (no
	// auth, which apiStartupGuard above already permits only with the opt-in);
	// a present token is always armed, and WithAPIToken itself fails closed at its
	// own layer: a whitespace-only/empty secret installs a reject-all authenticator
	// rather than leaving the API open. No layer may trust another to have caught it.
	apiOpts := []api.Option{
		api.WithLogger(log),
		api.WithWebhookReceiver(webhookReceiver),
		api.WithSSEHandler(sse),
	}
	if *apiToken != "" {
		apiOpts = append(apiOpts, api.WithAPIToken(*apiToken))
	}

	httpServer := &http.Server{
		Addr:              *httpAddr,
		Handler:           api.New(st, apiOpts...),
		ReadHeaderTimeout: 10 * time.Second,
	}

	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error {
		log.Info("gRPC runner service listening", "addr", *grpcAddr)
		if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("grpc serve: %w", err)
		}
		return nil
	})

	group.Go(func() error {
		log.Info("HTTP API listening", "addr", *httpAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http serve: %w", err)
		}
		return nil
	})

	group.Go(func() error {
		if err := dispatcher.Run(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("dispatcher: %w", err)
		}
		return nil
	})

	// The doorbell. Losing it is survivable — the dispatcher polls too — so a
	// failure here logs and retries rather than bringing the server down.
	group.Go(func() error {
		err := queue.Listen(groupCtx, st.DSN(), func(n queue.QueuedNotification) {
			log.Debug("job queued notification", "job_id", n.JobID, "tags", n.Tags)
			dispatcher.Wake()
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("queue listener stopped", "error", err)
		}
		return nil
	})

	group.Go(func() error {
		<-groupCtx.Done()
		log.Info("shutting down")

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()

		// GracefulStop lets in-flight runner streams finish rather than
		// severing every connected runner's session mid-job.
		stopped := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-shutdownCtx.Done():
			grpcServer.Stop()
		}

		return httpServer.Shutdown(shutdownCtx)
	})

	return group.Wait()
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// webhookStartupGuard refuses to start when no webhook signing secret is
// configured and the operator did not explicitly opt into the development-only
// fail-open path. It is the pure, testable core of the CIC-80 operational
// refusal; run() threads the flag values into it. Returns nil when a secret is
// present or when insecureWebhook is explicitly set.
func webhookStartupGuard(secret string, insecureWebhook bool) error {
	if secret == "" && !insecureWebhook {
		return errors.New("CICADO_WEBHOOK_SECRET is not set and --insecure-webhook was not passed; " +
			"set CICADO_WEBHOOK_SECRET for signature verification, or pass --insecure-webhook " +
			"(fail-open; for local development only)")
	}
	return nil
}

// envSecret is the single source of truth for every secret / token flag that
// keeps an empty default and falls back to a CICADO_* env var after flag.Parse().
// The flag value wins when non-empty (used exactly as typed, interior spaces
// preserved); an empty or whitespace-only flag falls back to os.Getenv(key) and
// the result is trimmed so a whitespace-only env var -- a Secret holding only a
// newline -- is treated as empty and fails closed (CIC-150 B4 / CIC-159 B1).
// run() calls it for each of the API/webhook/secret/github tokens and the
// database URL, and the tests call the same helper, so the trim cannot silently
// diverge between the two paths. Callers that need a "present vs empty" decision
// (fail-closed guards, --insecure-* opt-outs, the checks-reporter arming gate)
// compare the returned value against "".
func envSecret(flagVal, key string) string {
	if strings.TrimSpace(flagVal) != "" {
		return flagVal // the flag wins as typed.
	}
	return strings.TrimSpace(os.Getenv(key))
}

// resolveAPIToken is the API-token specialisation of envSecret: the empty-default
// --api-token flag falls back to CICADO_API_TOKEN. Kept as a named entry point so
// CIC-150 B4's run()/test path reads literally; it funnels through envSecret so
// there is exactly one place that resolves a flag value to its env override.
func resolveAPIToken(flagVal string) string {
	return envSecret(flagVal, "CICADO_API_TOKEN")
}

// resolveSecretFlags applies the env-var fallback for every secret / token flag
// that keeps an empty default. It is the single call run() makes after
// flag.Parse(): dropping the call from run() is caught by
// TestDBEnvFallbackIsWiredIntoRun (a missing database URL makes the start-up
// error fire); dropping any single assignment inside is caught by
// TestResolveSecretFlags, which exercises exactly this function on a fresh
// FlagSet and asserts every field is trimmed from its CICADO_* env var. No call
// site sees a different value than before: envSecret returns the operator's
// CICADO_* value verbatim (interior spaces preserved), or "" when the env var
// is absent.
func resolveSecretFlags(f *serverFlags) {
	*f.apiToken = resolveAPIToken(*f.apiToken)
	*f.databaseURL = envSecret(*f.databaseURL, "CICADO_DATABASE_URL")
	*f.webhookSecret = envSecret(*f.webhookSecret, "CICADO_WEBHOOK_SECRET")
	*f.secretKeyHex = envSecret(*f.secretKeyHex, "CICADO_SECRET_KEY")
	*f.githubWorkflowToken = envSecret(*f.githubWorkflowToken, "CICADO_GITHUB_WORKFLOW_TOKEN")
	*f.githubAppToken = envSecret(*f.githubAppToken, "CICADO_GITHUB_APP_TOKEN")
}

// apiStartupGuard refuses to start when no API token is configured and the
// operator did not explicitly opt into the development-only fail-open path. It
// is the operational half of CIC-150: a production server must not leave
// /api/v1 -- and therefore any job's live log stream -- readable by anyone who
// can reach the port. run() threads the flag values into it. Returns nil when a
// token is present or when --insecure-api was explicitly set.
func apiStartupGuard(apiToken string, insecureAPI bool) error {
	if apiToken == "" && !insecureAPI {
		return errors.New("CICADO_API_TOKEN is not set and --insecure-api was not passed; " +
			"set CICADO_API_TOKEN for bearer authentication on /api/v1/*, or pass --insecure-api " +
			"(fail-open; for local development only)")
	}
	return nil
}

func envBool(key string, fallback bool) bool {
	switch os.Getenv(key) {
	case "":
		return fallback
	case "0", "false", "no":
		return false
	default:
		return true
	}
}

// newWorkflowProducer builds the GitHub workflow producer from the
// --github-workflow-token value, or returns nil when the token is empty. The
// return is an untyped nil in that case, so the controller's
// `cfg.Producer != nil` check sees no producer (a typed nil *GitHubWorkflowProducer
// in the interface would not compare equal to nil).
func newWorkflowProducer(token string, st store.WorkflowStore, log *slog.Logger, httpClient *http.Client) controller.WorkflowProducer {
	if token == "" {
		return nil
	}
	return controller.NewGitHubWorkflowProducer(token, st, log, httpClient)
}

// newForge returns the forge the controller reports runs to: the GitHub
// Checks API when token is set, otherwise a NoOp forge (CIC-81). No token
// minter is wired yet, so the GitHub forge cannot mint clone credentials
// (CIC-146).
func newForge(token string, log *slog.Logger) forge.Forge {
	if token == "" {
		log.Warn("no --github-app-token: runs will not be reported to GitHub as checks")
		return controller.NoOpForge{}
	}
	return forge.NewGitHubForge(githubpkg.NewChecksReporterAdapter(githubpkg.NewChecksClient(token, nil)), nil)
}

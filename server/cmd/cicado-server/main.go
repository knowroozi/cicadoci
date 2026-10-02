// Command cicado-server is the CICaDo control plane.
//
// One binary: HTTP API, gRPC runner service, scheduler, and (from M6) the web
// UI embedded via embed.FS. Its only stateful dependency is Postgres.
//
// CIC-71d wires the controller (CIC-71a/b/c) into the webhook receiver.
// The controller is used as the webhook.Dispatcher for the receiver; a
// NoOp forge is used until CIC-71 lands the real GitHub forge.
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
	"cicadoci.com/cicado/server/audit"
	"cicadoci.com/cicado/server/controller"
	"cicadoci.com/cicado/server/dispatch"
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

func run() error {
	var (
		grpcAddr             = flag.String("grpc-addr", envOr("CICADO_GRPC_ADDR", ":8081"), "address for the gRPC runner service")
		httpAddr             = flag.String("http-addr", envOr("CICADO_HTTP_ADDR", ":8080"), "address for the HTTP API")
		databaseURL          = flag.String("database-url", os.Getenv("CICADO_DATABASE_URL"), "Postgres connection string")
		migrate              = flag.Bool("migrate", envBool("CICADO_MIGRATE", true), "apply pending migrations at startup")
		devSeedFlag          = flag.Bool("dev-seed", envBool("CICADO_DEV_SEED", false), "seed a development org, a well-known registration token, and a demo job (NEVER in production)")
		logLevel             = flag.String("log-level", envOr("CICADO_LOG_LEVEL", "info"), "debug, info, warn, or error")
		webhookSecret        = flag.String("webhook-secret", os.Getenv("CICADO_WEBHOOK_SECRET"), "GitHub App webhook signing secret; empty disables verification unless --insecure-webhook is also set")
		insecureWebhook      = flag.Bool("insecure-webhook", envBool("CICADO_INSECURE_WEBHOOK", false), "permit running with no webhook signing secret (fail-open); DEVELOPMENT ONLY — GitHub deliveries accepted without HMAC verification")
		webhookMaxConcurrent = flag.Int("webhook-max-concurrent", 16, "max concurrent webhook dispatches (flood backpressure); 0 disables")
		secretKeyHex         = flag.String("secret-key", os.Getenv("CICADO_SECRET_KEY"), "hex-encoded 32-byte AES-256 encryption key for the secrets store")
		allowEphemeralKey    = flag.Bool("allow-ephemeral-key", envBool("CICADO_ALLOW_EPHEMERAL_KEY", false), "permit an ephemeral random secret key when CICADO_SECRET_KEY is empty (DEVELOPMENT ONLY: secrets will not survive a restart)")
		githubWorkflowToken  = flag.String("github-workflow-token", os.Getenv("CICADO_GITHUB_WORKFLOW_TOKEN"), "long-lived fine-grained GitHub personal access token (Contents: read) used to fetch the repo's workflow files on a default-branch push; NOT a GitHub App installation token, which expires after 1 h (prefer the env var: a flag value shows in ps); empty keeps the producer off and relies on the store/dev-seed path")
	)
	flag.Parse()

	log := newLogger(*logLevel)
	slog.SetDefault(log)

	// Fail closed on a missing webhook signing secret unless the operator
	// explicitly opted in to the development-only fail-open path.  This is
	// the operational half of CIC-80: a production server with no webhook
	// secret must not start and accept unverified deliveries.
	if err := webhookStartupGuard(*webhookSecret, *insecureWebhook); err != nil {
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

	svcCfg := runnersvc.DefaultConfig()
	// CIC-103: the one run-terminal hook. The queue fires it exactly once per
	// run, after the transaction that finished the run's last job commits
	// (runner report, sweeper dead letter, or cancel scan alike). It must be
	// set here, before runnersvc.New copies svcCfg. forge.Complete and the
	// backpressure release attach to this hook rather than adding their own.
	svcCfg.OnRunTerminal = func(runID uuid.UUID, state string) {
		log.Info("run finished", "run_id", runID, "state", state)
	}
	svc := runnersvc.New(st.Pool(), q, svcCfg, log, secretsStore)
	dispatcher := dispatch.New(q, svc.Sessions(), dispatch.DefaultConfig(svcCfg.LeaseTTL()), log)

	// CIC-71d: wire the controller as the webhook dispatcher.
	//
	// NoOp forge is used until CIC-71 lands the real GitHub
	// Checks API + token-minter forge.  The controller logs a
	// warning if Prepare/Start/Complete fails, which is acceptable
	// for a no-op forge.
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
	// TODO(CIC-121): the fetch path is .github/workflows (the #95 default); the
	// .cicado/workflows convention is decided in CIC-121.
	if p := newWorkflowProducer(*githubWorkflowToken, st, log, &http.Client{Timeout: 20 * time.Second}); p != nil {
		ctrlOpts.Producer = p
		log.Info("workflow producer active",
			"source", "github", "token_source", "CICADO_GITHUB_WORKFLOW_TOKEN")
	}

	ctrl := controller.New(st, controller.NoOpForge{}, ctrlOpts, log)

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
	httpServer := &http.Server{
		Addr:              *httpAddr,
		Handler:           api.New(st, api.WithLogger(log), api.WithWebhookReceiver(webhookReceiver)),
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

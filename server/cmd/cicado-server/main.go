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
	"strconv"
	"strings"
	"sync"
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
	"cicadoci.com/cicado/server/approvalgates"
	"cicadoci.com/cicado/server/archive"
	"cicadoci.com/cicado/server/audit"
	"cicadoci.com/cicado/server/blobstore"
	"cicadoci.com/cicado/server/chat"
	"cicadoci.com/cicado/server/controller"
	"cicadoci.com/cicado/server/dispatch"
	"cicadoci.com/cicado/server/forge"
	githubpkg "cicadoci.com/cicado/server/github"
	"cicadoci.com/cicado/server/logs"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/redact"
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
	githubAppID          *int64
	githubAppKeyFile     *string
	// CIC-160 B1: Slack channel for approval-gate messages (empty = no gate, B2 fail-closed).
	approvalChannel *string
	// CIC-160 B1/F1: gate auto-timeout; 0s = no auto-timeout.
	approvalTimeout *time.Duration
	// CIC-162: approvalgates Manager wiring. The channel and timeout come from
	// CIC-160's --approval-channel / --approval-timeout above.
	approvalgatesEnabled      *bool
	approvalgatesAllowed      *string
	insecureApprovalgatesNoop *bool
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
		// CIC-151 / CIC-146: per-job GitHub installation token minting.
		// When --github-app-id and --github-app-key-file are both set, a
		// per-job minter is wired into runnersvc. The App private key
		// path is NEVER logged or written to the database.
		githubAppID:      fs.Int64("github-app-id", int64(0), "GitHub App ID for App-JWT minting (CIC-151); 0 disables per-job token minting"),
		githubAppKeyFile: fs.String("github-app-key-file", "", "path to GitHub App PEM private key (RSA only, PKCS#1 or PKCS#8); used with --github-app-id; NEVER logged or persisted"),
		// CIC-160 B1: approvalgates Slack channel + gate auto-timeout.
		approvalChannel: fs.String("approval-channel", envOr("CICADO_APPROVAL_CHANNEL", ""), "Slack channel for approval-gate messages; empty means no gate is opened (B2 fail-closed)"),
		approvalTimeout: fs.Duration("approval-timeout", 0, "gate auto-timeout; 0s = no auto-timeout; CICADO_APPROVAL_TIMEOUT env overrides"),
		// CIC-162 (part 1): build the approvalgates Manager. No real ChatProvider
		// exists yet, so enabling it also requires the explicit development
		// acknowledgement below (B4: fail closed in production).
		approvalgatesEnabled: fs.Bool("approvalgates-enabled", envBool("CICADO_APPROVALGATES_ENABLED", false),
			"build the approval-gate manager and open a gate for every PR run (CIC-162 part 1; gates do not yet block dispatch); requires --insecure-approvalgates-noop until a real chat provider exists"),
		approvalgatesAllowed: fs.String("approvalgates-allowed", envOr("CICADO_APPROVALGATES_ALLOWED", ""),
			"comma-separated user IDs authorized to resolve approval gates; empty = fail closed, no one authorized (CIC-162)"),
		insecureApprovalgatesNoop: fs.Bool("insecure-approvalgates-noop", envBool("CICADO_INSECURE_APPROVALGATES_NOOP", false),
			"permit --approvalgates-enabled with the no-op chat provider: gates post nothing and nothing can resolve them (DEVELOPMENT ONLY -- CIC-162)"),
	}
}

func run() error {
	// Registration lives in registerFlags(fs) so the flag set is registered
	// once and the B1-test can exercise it on a fresh FlagSet; run() uses the
	// returned pointers under the same bare names as before.
	f := registerFlags(flag.CommandLine)
	grpcAddr, httpAddr, databaseURL, migrate, devSeedFlag, logLevel, webhookSecret,
		insecureWebhook, webhookMaxConcurrent, secretKeyHex, allowEphemeralKey,
		githubWorkflowToken, githubAppToken, apiToken, insecureAPI,
		githubAppID, githubAppKeyFile, approvalChannel, approvalTimeout :=
		f.grpcAddr, f.httpAddr, f.databaseURL, f.migrate, f.devSeedFlag, f.logLevel,
		f.webhookSecret, f.insecureWebhook, f.webhookMaxConcurrent, f.secretKeyHex,
		f.allowEphemeralKey, f.githubWorkflowToken, f.githubAppToken, f.apiToken, f.insecureAPI,
		f.githubAppID, f.githubAppKeyFile, f.approvalChannel, f.approvalTimeout

	flag.Parse()

	// CIC-150 B1/B4 / CIC-159 B1: resolve all six secret / token flags through the
	// SAME helper the tests call (resolveSecretFlags below, which funnels through
	// envSecret). run() calls this once so dropping the call from run() is caught
	// by TestDBEnvFallbackIsWiredIntoRun; dropping any single assignment in
	// resolveSecretFlags is caught by TestResolveSecretFlags. No behaviour change
	// for a non-empty env value: it was read at registration time before this
	// change.
	if err := resolveSecretFlags(f); err != nil {
		return err
	}

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
	// CIC-151 / CIC-146: wire the per-job GitHub installation token
	// minter. When --github-app-id and --github-app-key-file are both
	// set, GetJobSecrets dynamically mints a short-lived token per job;
	// when either is empty the minter is nil and GetJobSecrets treats
	// "github_clone_token" as a static secret looked up from the store.
	var minter runnersvc.CloneMinterFunc
	if *githubAppID > 0 && *githubAppKeyFile != "" {
		keyBytes, err := os.ReadFile(*githubAppKeyFile)
		if err != nil {
			return fmt.Errorf("read --github-app-key-file: %w", err)
		}
		// B3a: the minter requires a redact.RegisteringMasker so the
		// minted token value is masked before it reaches log storage
		// or the gRPC response. A misconfigured masker is a fatal error.
		rm, ok := svcCfg.Masker.(redact.RegisteringMasker)
		if !ok {
			return fmt.Errorf("CIC-151: clone minter requires redact.RegisteringMasker, got %T", svcCfg.Masker)
		}
		// B3b: validate the key at startup so a malformed key fails the
		// binary immediately rather than producing opaque errors when the
		// first job that needs a token tries to mint it. No HTTP call is
		// made here; just a local RSA PKCS#1v15 sign with the key bytes.
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, probeErr := githubpkg.AppAuthFunc(probeCtx, githubpkg.InstallationConfig{
			AppID:         *githubAppID,
			AppPrivateKey: keyBytes,
		}); probeErr != nil {
			probeCancel()
			return fmt.Errorf("CIC-151: --github-app-key-file key validation failed (expected PKCS#1 or PKCS#8 RSA PEM): %w", probeErr)
		}
		probeCancel()

		// B2: per-repo resolver + per-repo scoping via repos.installation_id.
		minter = runnersvc.NewCloneMinterFunc(
			context.Background(), st.Pool(), *githubAppID, keyBytes, rm)
	}

	svcCfg.CloneMinter = minter
	if minter != nil {
		// B3a: log app_id only; do NOT log the key file path.
		log.Info("CIC-151: GitHub clone minter enabled (per-repo, RS256)",
			"app_id", *githubAppID)
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
	// CIC-160 B1/F1: wire approvalgates channel + timeout.
	// CICADO_APPROVAL_TIMEOUT env overrides the flag; unparseable or
	// negative values refuse startup (Dwight F1 last-fix).
	ctrlOpts.ApprovalChannel = *approvalChannel
	timeout, err := resolveApprovalTimeout(*approvalTimeout, os.Getenv("CICADO_APPROVAL_TIMEOUT"))
	if err != nil {
		return err
	}
	ctrlOpts.ApprovalTimeout = timeout
	// Wire the store-backed workflow loader so runs produce job DAGs.
	// With no loader, LoadAndCreateJobs is a silent no-op and the server accepts
	// webhooks but never creates jobs (the finding CIC-78 closed). StoreLoader
	// reads the workflows table; the production producer that fills the table is
	// the GitHub-raw fetch, tracked as its own follow-up producer ticket (NOT
	// CIC-38/39/40, which are the secrets RPC / OIDC issuer / job tokens), or a
	// dev-seed / CLI ingest in dev. A nil table simply means no jobs, which is
	// the safe default.
	ctrlOpts.WorkflowLoader = controller.NewStoreLoader(st, log)

	// CIC-180: wire the per-call installation-token minter into the workflow
	// producer (CIC-146 P2). When --github-app-id and --github-app-key-file
	// are configured, each Produce call mints a short-lived installation
	// access token scoped to the pushed-to repo via
	// github.InstallationTokenMinter + repos.installation_id. The static
	// fine-grained PAT (--github-workflow-token / CICADO_GITHUB_WORKFLOW_TOKEN)
	// is a fallback for environments that do not use the GitHub App. PRs
	// never invoke the producer (gates live in the push/PR handler), so no
	// fork-supplied definition reaches the shared repo row. An unavailable
	// minter + empty token leaves the producer nil (store/dev-seed path).
	// B2: single builder — minter wins, PAT is fallback, untyped nil when neither.
	httpClientProd := &http.Client{Timeout: 20 * time.Second}
	ctrlOpts.Producer = newWorkflowProducer(minter, *githubWorkflowToken, st, log, httpClientProd)

	// CIC-162 (part 1): approval gates. buildApprovalGates refuses
	// --approvalgates-enabled without --insecure-approvalgates-noop, because the
	// only ChatProvider today is the no-op one (a real Slack provider is
	// CIC-138). Channel and timeout come from CIC-160's ApprovalChannel /
	// ApprovalTimeout above.
	approvalMgr, approvers, waitGateAudit, err := buildApprovalGates(ctx, *f.approvalgatesEnabled,
		*f.insecureApprovalgatesNoop, *f.approvalgatesAllowed, auditLog, log)
	if err != nil {
		return err
	}
	if approvalMgr != nil {
		ctrlOpts.ApprovalGates = approvalMgr
		ctrlOpts.AllowedApprovers = approvers
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
	// CIC-150 B4 + CIC-194 S1: the operator write surface (secrets
	// PUT/DELETE + job cancel) is armed ONLY when a real token is present,
	// via apiAuthOpts (extracted so it is table-tested on both branches).
	apiOpts = append(apiOpts, apiAuthOpts(*apiToken, api.WriteDeps{
		SecretStore: secretsStore,
		Cancel:      q.RequestCancel,
		CancelRun: func(ctx context.Context, runID uuid.UUID, reason string) (bool, error) {
			// fireTerminal releases the run's backpressure slot and runs
			// RunCompleted in a child goroutine (matching the OnRunTerminal
			// pattern at main.go:286) so it does not block the HTTP
			// request on a GitHub round-trip. CancelRunJobs calls it
			// after the tx commit ONLY when it finalized the run
			// (gated/pending case); the active-job case is handled by
			// advanceRun.
			fireTerminal := func(_ context.Context, rid uuid.UUID) {
				ctrl.ReleaseRun(rid)
				go func() {
					ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel2()
					if err := ctrl.RunCompleted(ctx2, rid, "cancelled"); err != nil {
						log.Warn("run-terminal hook after cancel failed", "run_id", rid, "error", err)
					}
				}()
			}
			return st.CancelRunJobs(ctx, runID, reason,
				func(ctx context.Context, jobID uuid.UUID, reason string) error {
					_, err := q.RequestCancel(ctx, jobID, reason)
					return err
				},
				fireTerminal,
			)
		},
		Audit: auditLog,
	})...)

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

		// CIC-204 r3 shutdown order:
		//       1. HTTP shutdown (stop accepting new webhooks).
		//       2. Drain pending approval gates (ReasonShutdownDrain).
		//       3. Join in-flight gate-dispatch goroutines (WaitGateDispatch).
		//       4. Drain in-flight gate-audit writes (waitGateAudit, CIC-193 G1).
		//       5. gRPC GracefulStop LAST so a runner stream can't
		//          block the 20 s grace window.

		sdErr := httpServer.Shutdown(shutdownCtx)
		if sdErr != nil {
			log.Error("httpServer.Shutdown returned an error", "err", sdErr)
		}

		if approvalMgr != nil {
			drained := approvalMgr.DrainAll(shutdownCtx)
			if drained > 0 {
				log.Info("drained pending approval gates at shutdown", "gates", drained)
			}
		}

		if ctrl != nil {
			ctrl.WaitGateDispatch(shutdownCtx)
		}

		// CIC-193 G1: a gate timer may fire during shutdown and dispatch
		// an audit write that has not yet completed. Drain before the
		// deferred DB pool close. No-op when gates are disabled.
		waitGateAudit()

		// 5. gRPC GracefulStop LAST, bounded.
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

		return sdErr
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

// resolveApprovalTimeout applies the CICADO_APPROVAL_TIMEOUT env override
// over the flag default.  An unparseable or negative value — whether
// from the env var or the flag — returns an error so run() can refuse
// to start (Dwight F1 last-fix: a silent "never time out" is a safety issue).
func resolveApprovalTimeout(flagVal time.Duration, override string) (time.Duration, error) {
	if override != "" {
		d, err := time.ParseDuration(override)
		if err != nil {
			return 0, fmt.Errorf("CICADO_APPROVAL_TIMEOUT=%q: %w", override, err)
		}
		if d < 0 {
			return 0, fmt.Errorf("CICADO_APPROVAL_TIMEOUT=%q: must not be negative", override)
		}
		return d, nil
	}
	if flagVal < 0 {
		return 0, fmt.Errorf("--approval-timeout=%q: must not be negative", flagVal)
	}
	return flagVal, nil
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
func resolveSecretFlags(f *serverFlags) error {
	*f.apiToken = resolveAPIToken(*f.apiToken)
	*f.databaseURL = envSecret(*f.databaseURL, "CICADO_DATABASE_URL")
	*f.webhookSecret = envSecret(*f.webhookSecret, "CICADO_WEBHOOK_SECRET")
	*f.secretKeyHex = envSecret(*f.secretKeyHex, "CICADO_SECRET_KEY")
	*f.githubWorkflowToken = envSecret(*f.githubWorkflowToken, "CICADO_GITHUB_WORKFLOW_TOKEN")
	*f.githubAppToken = envSecret(*f.githubAppToken, "CICADO_GITHUB_APP_TOKEN")
	// CIC-181: per-job App-JWT minter env fallbacks.
	appID, err := envInt64Or(*f.githubAppID, "CICADO_GITHUB_APP_ID")
	if err != nil {
		return err
	}
	*f.githubAppID = appID
	*f.githubAppKeyFile = envSecret(*f.githubAppKeyFile, "CICADO_GITHUB_APP_KEY_FILE")
	return nil
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

// apiAuthOpts builds the token-authenticated option set for the API server and
// is the single place CIC-194 S1 is enforced: the operator write surface
// (secrets PUT/DELETE + job cancel) is armed ONLY when a real token is present.
// An --insecure-api server (no token) therefore gets no write routes at all, so
// the credential-less operator surface never exists. A token present returns the
// write-deps option plus the token authenticator, in that order. Extracted from
// run() so both branches are table-tested (TestApiAuthOpts, R4).
func apiAuthOpts(apiToken string, deps api.WriteDeps) []api.Option {
	if apiToken == "" {
		return nil
	}
	return []api.Option{
		api.WithWriteDeps(deps),
		api.WithAPIToken(apiToken),
	}
}

// envInt64Or resolves a flag value with a CICADO_* env-var fallback.
// If the flag value is non-zero it wins as typed. Otherwise the env var
// is parsed via strconv.ParseInt; a parse error or a negative value is
// returned as an error so run() can fail-closed (CIC-181 B3).
func envInt64Or(flagVal int64, key string) (int64, error) {
	if flagVal != 0 {
		return flagVal, nil
	}
	v := os.Getenv(key)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: %w", key, v, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid %s=%d: must be non-negative", key, n)
	}
	return n, nil
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

// newForge returns the forge the controller reports runs to: the GitHub
// Checks API when a token is set, otherwise a NoOp forge (CIC-81).  Clone
// credential minting (CIC-151 / CIC-180) is handled by runnersvc, not the
// forge.
func newForge(token string, log *slog.Logger) forge.Forge {
	if token == "" {
		log.Warn("no --github-app-token: runs will not be reported to GitHub as checks")
		return controller.NoOpForge{}
	}
	return forge.NewGitHubForge(githubpkg.NewChecksReporterAdapter(githubpkg.NewChecksClient(token, nil)))
}

// newWorkflowProducer wires the controller's workflow producer.
// If a CloneMinterFunc (GitHub App installer) is supplied, it creates a producer that uses the minter (CIC‑180).
// Otherwise, if a static token is given, it falls back to a static‑PAT producer (CIC‑120).
func newWorkflowProducer(
	minter runnersvc.CloneMinterFunc,
	token string,
	st store.WorkflowStore,
	log *slog.Logger,
	httpClient *http.Client,
) controller.WorkflowProducer {
	// CIC-180: per-call GitHub App installation-token minter.
	// The runner's CloneMinterFunc is adapted to the controller's
	// MinterFunc signature (repoID only; orgID and isFork are dropped
	// since the resolver looks up installation_id by repoID and the
	// producer is only called for non-fork push events).
	if minter != nil {
		prodMinter := controller.MinterFunc(func(ctx context.Context, repoID string) (string, error) {
			return minter(ctx, "", repoID, false)
		})
		log.Info("workflow producer active (GitHub App minter)",
			"source", "github", "token_source", "CICADO_GITHUB_APP_ID")
		return controller.NewGitHubWorkflowProducerWithMinter(prodMinter, st, log, httpClient)
	}
	// CIC-120: static-PAT fallback.
	if token == "" {
		return nil
	}
	log.Info("workflow producer active (static PAT)",
		"source", "github", "token_source", "CICADO_GITHUB_WORKFLOW_TOKEN")
	return controller.NewGitHubWorkflowProducer(token, st, log, httpClient)
}

// splitAllowList parses a comma-separated CSV of user IDs into a slice.
// Empty csv => nil, so the gate open-list is empty = fail closed (CIC-162).
func splitAllowList(csv string) []string {
	if csv == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(csv, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// buildApprovalGates constructs the approvalgates Manager for
// --approvalgates-enabled (CIC-162 part 1). It returns (nil, nil, nil) when
// gates are disabled.
//
// Only a no-op ChatProvider exists today, so gates post nothing and nothing
// can resolve them before they time out. Enabling gates therefore also
// requires noopAck (--insecure-approvalgates-noop): without it startup is
// refused, so a production deploy cannot silently run inert gates (#135 B4).
func buildApprovalGates(ctx context.Context, enabled, noopAck bool, allowedCSV string,
	auditLog audit.Log, log *slog.Logger) (*approvalgates.Manager, []string, func(), error) {
	if !enabled {
		// No gates, no in-flight audit writes to drain.
		return nil, nil, func() {}, nil
	}
	if !noopAck {
		return nil, nil, nil, errors.New("--approvalgates-enabled has no real chat provider yet (CIC-138); " +
			"gates would post nothing and could never be resolved. Pass --insecure-approvalgates-noop " +
			"(CICADO_INSECURE_APPROVALGATES_NOOP=true) to run them inert for development")
	}
	var mgr *approvalgates.Manager
	// CIC-193 G1: a gate transition can reach onResolved on a timer goroutine
	// (time.AfterFunc) or from a resolve request AFTER the main shutdown
	// flow has moved on; a pool close could race such an in-flight write. The
	// close of the manager and the join of any in-flight writes are guarded by
	// one mutex so that the check-then-Add below is atomic with the
	// wait-func: a resolution that lands after closing flips runs the audit
	// write synchronously and cannot Add() after the waiter has returned.
	// waitGateAudit turns the gate on for shutdown and joins the pre-shutdown
	// in-flight writes; the shutdown path calls it after httpServer.Shutdown
	// and before the DB pool closes. It is a no-op when gates are disabled or
	// auditLog is nil. CloseAll itself is a load-failure path (controller.go),
	// not a SIGTERM handler -- the timer-fires-after-shutdown and
	// abandoned-pending cases are tracked as CIC-204, out of scope here.
	var mu sync.Mutex
	var closing bool
	var drainWG sync.WaitGroup
	onResolved := func(runID string, state approvalgates.GateState, reason string) {
		e := gateAuditEntry(runID, state, mgr.ActorID(runID), reason)
		log.Info("approval gate resolved", "run", runID, "state", state, "actor", e.Actor, "reason", reason)
		if auditLog == nil {
			return
		}
		mu.Lock()
		if closing {
			// Shutdown has begun: run the write synchronously so the transition is
			// not lost and cannot Add() after the waiter has returned. The write
			// itself is bounded by appendGateAudit's own timeout so a hung pool
			// connection cannot block shutdown.
			mu.Unlock()
			appendGateAudit(ctx, auditLog, e, log, runID)
			return
		}
		drainWG.Add(1)
		mu.Unlock()
		defer drainWG.Done()
		appendGateAudit(ctx, auditLog, e, log, runID)
	}
	// waitGateAudit is safe to call in the shutdown path; it is a no-op when
	// gates are disabled (the !enabled early return yields a no-op func).
	waitGateAudit := func() {
		mu.Lock()
		closing = true // under the same lock as onResolved's check-then-Add
		mu.Unlock()
		drainWG.Wait()
	}
	mgr = approvalgates.NewManager(chat.NewNoOpProvider(), onResolved)
	approvers := splitAllowList(allowedCSV)
	if len(approvers) == 0 {
		log.Warn("approvalgates: enabled but --approvalgates-allowed is empty; gates open but no one is authorized to resolve them")
	}
	log.Warn("approvalgates enabled with the NO-OP chat provider: gates are inert (nothing is posted, nothing resolves them, dispatch is not blocked)",
		"allow", approvers)
	return mgr, approvers, waitGateAudit, nil
}

// gateAuditEntry maps a terminal gate transition to its audit entry. The
// Action reflects the outcome, so a consumer filtering on action = 'approve'
// sees approvals only (#135 B2). A timeout has no acting user and is recorded
// as "system". The reason string is folded into the entry's Reason field when
// non-empty, so a shutdown drain (ReasonShutdownDrain = "abandoned: server
// shutdown") is audited distinctly from a normal auto-timeout that reads
// ReasonApprovalTimedOut = "approval timed out" (CIC-204 B1).
func gateAuditEntry(runID string, state approvalgates.GateState, actorID, reason string) audit.Entry {
	action := audit.ActionGateTimeout
	// Principal + surface depend on the outcome: a human's approval or denial is a
	// user acting over the chat surface; a timeout (and any future non-user
	// terminal state) is a system event with no acting user. Keep these in sync
	// with the per-state unit test TestGateAuditEntry.*.
	principal, surface := audit.PrincipalSystem, audit.SurfaceSystem
	switch state {
	case approvalgates.GateApproved:
		action = audit.ActionApprove
		principal, surface = audit.PrincipalUser, audit.SurfaceChat
	case approvalgates.GateDenied:
		action = audit.ActionDeny
		principal, surface = audit.PrincipalUser, audit.SurfaceChat
	default:
		// GateTimedOut and any future system-initiated terminal state.
	}
	actor := actorID
	if principal == audit.PrincipalSystem || actor == "" {
		// A timeout (and any system-initiated terminal) has no acting user, so the
		// Actor column records "system" rather than a blank principal id.
		actor = "system"
	}
	// Fold the terminal reason into the entry's reason field. An empty reason keeps the "gate <state>" form for a user resolve; a non-empty reason (from a shutdown drain) is the one that matters (CIC-204 B1).
	r := reason
	if r == "" {
		r = "gate " + state.String()
	}
	return audit.Entry{
		Action:        action,
		Actor:         actor,
		Target:        runID,
		PrincipalType: principal,
		Surface:       surface,
		Reason:        r,
	}
}

// appendGateAudit records a terminal approval-gate transition to the audit log
// on a context that is DETACHED from the signal context but BOUNDED by a 5s
// timeout. A gate can reach a terminal state -- and therefore fire onResolved --
// during graceful shutdown: a gate timeout whose timer fires just after shutdown
// began, or a gate resolve that lands as the HTTP server drains. If it used the
// signal-canceled request context directly, the append would fail with
// context.Canceled and the terminal transition would go unaudited. Detaching
// (context.WithoutCancel) lets the audit write survive the SIGTERM; the 5s
// timeout mirrors the other bounded shutdown contexts in this program (the 20s
// graceful-shutdown ctx, runnersvc's offline ctx) so a hung DB connection cannot
// block shutdown. It is a no-op when auditLog is nil. The wiring's drain
// wait-func joins an in-flight write so the Postgres pool is not closed mid-
// write. CloseAll is a load-failure path (controller.go), not a SIGTERM handler;
// the timer-fires-after-shutdown and abandoned-pending cases are tracked as
// CIC-204, out of scope here.
func appendGateAudit(ctx context.Context, auditLog audit.Log, e audit.Entry, log *slog.Logger, runID string) {
	if auditLog == nil {
		return
	}
	appendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := auditLog.Append(appendCtx, e); err != nil {
		log.Error("approvalgates: audit append failed", "run", runID, "err", err)
	}
}

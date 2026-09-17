// Command cicado-server is the CICaDo control plane.
//
// One binary: HTTP API, gRPC runner service, scheduler, and (from M6) the web
// UI embedded via embed.FS. Its only stateful dependency is Postgres.
package main

import (
	"context"
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

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
	"cicadoci.com/cicado/server/dispatch"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/runnersvc"
	"cicadoci.com/cicado/server/store"
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
		grpcAddr    = flag.String("grpc-addr", envOr("CICADO_GRPC_ADDR", ":8081"), "address for the gRPC runner service")
		httpAddr    = flag.String("http-addr", envOr("CICADO_HTTP_ADDR", ":8080"), "address for the HTTP API")
		databaseURL = flag.String("database-url", os.Getenv("CICADO_DATABASE_URL"), "Postgres connection string")
		migrate     = flag.Bool("migrate", envBool("CICADO_MIGRATE", true), "apply pending migrations at startup")
		devSeedFlag = flag.Bool("dev-seed", envBool("CICADO_DEV_SEED", false), "seed a development org, a well-known registration token, and a demo job (NEVER in production)")
		logLevel    = flag.String("log-level", envOr("CICADO_LOG_LEVEL", "info"), "debug, info, warn, or error")
	)
	flag.Parse()

	log := newLogger(*logLevel)
	slog.SetDefault(log)

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

	q := queue.NewPostgres(st.Pool())

	if *devSeedFlag {
		if err := devSeed(ctx, st, q, log); err != nil {
			return err
		}
	}

	svcCfg := runnersvc.DefaultConfig()
	svc := runnersvc.New(st.Pool(), q, svcCfg, log)
	dispatcher := dispatch.New(q, svc.Sessions(), dispatch.DefaultConfig(svcCfg.LeaseTTL()), log)

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

	httpServer := &http.Server{
		Addr:              *httpAddr,
		Handler:           newHTTPHandler(st),
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

// newHTTPHandler serves the health endpoints. The API proper arrives with M1.
func newHTTPHandler(st *store.Store) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Readiness depends on the database: a control plane that cannot reach
	// Postgres cannot schedule anything, so it should not receive traffic.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := st.Pool().Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("database unreachable\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})

	return mux
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

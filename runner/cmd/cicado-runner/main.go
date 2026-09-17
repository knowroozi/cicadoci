// Command cicado-runner is the CICaDo runner agent.
//
// It enrolls once with a registration token, persists the credential it is
// given, and thereafter dials out to the server and waits for work. Nothing
// ever connects to it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"cicadoci.com/cicado/runner"
)

// identity is what a runner persists between restarts.
type identity struct {
	RunnerID   string   `json:"runner_id"`
	Credential string   `json:"credential"`
	Labels     []string `json:"labels"`
	ServerAddr string   `json:"server_address"`
}

func main() {
	if err := run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "cicado-runner: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		serverAddr = flag.String("server", envOr("CICADO_SERVER", "localhost:8081"), "server gRPC address (host:port)")
		regToken   = flag.String("registration-token", os.Getenv("CICADO_REGISTRATION_TOKEN"), "one-time registration token (first run only)")
		statePath  = flag.String("state", envOr("CICADO_RUNNER_STATE", "/var/lib/cicado/runner.json"), "where to persist runner identity")
		name       = flag.String("name", envOr("CICADO_RUNNER_NAME", defaultName()), "operator-facing runner name")
		workspace  = flag.String("workspace", envOr("CICADO_WORKSPACE_ROOT", "/var/lib/cicado/workspaces"), "root directory for job workspaces")
		capacity   = flag.Int("capacity", 1, "how many jobs to run concurrently")
		executors  = flag.String("executors", envOr("CICADO_EXECUTORS", "shell"), "comma-separated executors to offer")
		insecureTL = flag.Bool("insecure", envBool("CICADO_INSECURE", false), "connect without TLS (development only)")
		logLevel   = flag.String("log-level", envOr("CICADO_LOG_LEVEL", "info"), "debug, info, warn, or error")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	executorList := splitAndTrim(*executors)
	if len(executorList) == 0 {
		return errors.New("no executors configured")
	}

	conn, err := dial(*serverAddr, *insecureTL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	id, err := loadIdentity(*statePath)
	switch {
	case err == nil:
		log.Info("using stored identity", "runner_id", id.RunnerID, "state", *statePath)

	case errors.Is(err, os.ErrNotExist):
		if *regToken == "" {
			return fmt.Errorf("no stored identity at %s and no registration token given; "+
				"pass -registration-token or set CICADO_REGISTRATION_TOKEN", *statePath)
		}
		log.Info("enrolling with server", "server", *serverAddr, "name", *name)

		enrollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		resp, enrollErr := runner.Enroll(enrollCtx, conn, *regToken, *name, executorList)
		cancel()
		if enrollErr != nil {
			return enrollErr
		}
		id = identity{
			RunnerID:   resp.GetRunnerId(),
			Credential: resp.GetRunnerCredential(),
			Labels:     resp.GetLabels(),
			ServerAddr: *serverAddr,
		}
		if err := saveIdentity(*statePath, id); err != nil {
			return err
		}
		log.Info("enrolled", "runner_id", id.RunnerID, "labels", id.Labels)

	default:
		return err
	}

	agent := runner.New(runner.Config{
		ServerAddress: *serverAddr,
		Credential:    id.Credential,
		RunnerID:      id.RunnerID,
		WorkspaceRoot: *workspace,
		Capacity:      *capacity,
		Executors:     executorList,
		Logger:        log,
	}, conn)

	log.Info("runner starting",
		"runner_id", id.RunnerID, "server", *serverAddr,
		"capacity", *capacity, "executors", executorList)

	return agent.Run(ctx)
}

func dial(addr string, allowInsecure bool) (*grpc.ClientConn, error) {
	if !allowInsecure {
		// The protocol is gRPC over TLS with no plaintext fallback (protocol
		// §11). Refusing here rather than silently downgrading means a
		// misconfigured production runner fails loudly instead of shipping job
		// output and credentials in the clear.
		return nil, errors.New("TLS transport credentials are not configured yet (CIC-13); " +
			"pass -insecure for local development against a development server")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return conn, nil
}

// loadIdentity reads the persisted runner identity.
func loadIdentity(path string) (identity, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-specified state path
	if err != nil {
		return identity{}, err
	}
	var id identity
	if err := json.Unmarshal(data, &id); err != nil {
		return identity{}, fmt.Errorf("parse runner state at %s: %w", path, err)
	}
	if id.Credential == "" || id.RunnerID == "" {
		return identity{}, fmt.Errorf("runner state at %s is incomplete", path)
	}
	return id, nil
}

// saveIdentity persists the credential at 0600. It holds a bearer secret that
// authorizes claiming jobs, so it is written no more widely than that.
func saveIdentity(path string, id identity) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runner state: %w", err)
	}
	// Write-then-rename so a crash mid-write cannot leave a truncated
	// credential behind, which would need a fresh registration token to fix.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write runner state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install runner state: %w", err)
	}
	return nil
}

func defaultName() string {
	host, err := os.Hostname()
	if err != nil {
		return "cicado-runner"
	}
	return host
}

func splitAndTrim(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func parseLevel(level string) slog.Level {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return slog.LevelInfo
	}
	return lv
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

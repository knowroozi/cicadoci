package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// CIC-159 B1: the fallback test below drives the SAME envSecret helper run() calls
// for every secret / token flag, so its expectation cannot silently diverge from
// run()'s wiring. The leak test below builds the real binary and asserts none of
// the CICADO_* secret values leak into the program's flag usage text. The DB-wiring
// test proves run() actually makes the resolveSecretFlags call at start-up. The
// bounded context per sub-process guards against a hang; a bounded per-command
// context satisfies the noctx linter.

// TestSecretFlagValuesNotInFlagUsage builds the real cicado-server binary in a
// throwaway build dir and inspects its output under -h, --help, and an
// unrecognised flag, with EVERY CICADO_* secret env var set (incl. CICADO_API_TOKEN).
// Because -h is intercepted before flag.Parse(), the values must be absent from
// usage text. It then runs -h with no env vars and confirms clean usage.
//
// This is a binary smoke test, not a stub FlagSet: it catches a real `os.Getenv`
// default creeping back into any secret / token flag without a new unit test having
// to be written for it.
func TestSecretFlagValuesNotInFlagUsage(t *testing.T) {
	bin := buildSecretServerBinary(t, "cicado-server-flag-leak-*")
	// Every secret / token env var, each carrying a distinctive sentinel that is
	// highly unlikely to appear in the program's own text.
	secrets := map[string]string{
		"CICADO_API_TOKEN":             "SENTINEL-APITK-7f3a9c0e",
		"CICADO_WEBHOOK_SECRET":        "SENTINEL-WHSEC-4410b2de",
		"CICADO_SECRET_KEY":            "SENTINEL-SECKEY-5567aa1b",
		"CICADO_GITHUB_WORKFLOW_TOKEN": "SENTINEL-GHWF-a1b2c3d4",
		"CICADO_GITHUB_APP_TOKEN":      "SENTINEL-GHAPP-ff00ee88",
		// Not a secret per se, but a DSN carries a DB password; same leak class.
		"CICADO_DATABASE_URL": "postgres://user:SENTINEL-DBPWD-9b8a7f6e@db-host:5432/cicado",
	}
	modes := []struct {
		name string
		args []string
	}{
		{"-h", []string{"-h"}},
		{"--help", []string{"--help"}},
		{"--bogus", []string{"--this-flag-does-not-exist"}},
	}
	for _, mode := range modes {
		for key, val := range secrets {
			t.Run(mode.name+"/"+key, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, bin, mode.args...)
				cmd.Env = append(os.Environ(), key+"="+val)
				var buf bytes.Buffer
				cmd.Stdout = &buf
				cmd.Stderr = &buf
				// -h / --help exit 0; an unrecognised flag exits 2 (flag.ExitOnError)
				// and a startup guard exits 1 -- any of these just means the binary
				// ran far enough to print its usage text, which is what we assert on.
				// A non-exit error (e.g. the binary cannot launch) is a real failure.
				if err := cmd.Run(); err != nil {
					var exerr *exec.ExitError
					if !errors.As(err, &exerr) {
						t.Fatalf("binary failed to run: %v", err)
					}
				}
				out := buf.String()
				// Proves the binary reached its usage-text printer (so the leak check
				// below is actually exercising the right code path, not a no-op exit).
				if !strings.Contains(out, "Usage") && !strings.Contains(out, "usage") {
					t.Fatalf("binary did not print its usage text:\n%s", out)
				}
				if bytes.Contains(buf.Bytes(), []byte(val)) {
					t.Errorf("leaked env value %s=%q into flag usage output:\n%s",
						key, val, out)
				}
			})
		}
	}

	// Also confirm the default-mode (no env) run prints a usable -h message
	// (sanity regression: empty defaults should not panic the usage printer).
	{
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "-h")
		cmd.Env = os.Environ()
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Run(); err != nil {
			var exerr *exec.ExitError
			if !errors.As(err, &exerr) {
				t.Fatalf("-h run: %v", err)
			}
		}
		if !bytes.Contains(buf.Bytes(), []byte("database-url")) {
			t.Fatalf("default -h output missing --database-url; output:\n%s", buf.String())
		}
	}
}

// TestSecretEnvFallbackAfterParse exercises the SAME envSecret helper run() calls
// for every secret / token flag, so a regressed fallback or a dropped TrimSpace in
// the helper (or in run()'s wiring) is caught. It does NOT re-implement the trim
// in-test (the CIC-159 B1 violation) -- it calls envSecret directly with the exact
// shape run() uses: an empty flag val plus a CICADO_* env var.
func TestSecretEnvFallbackAfterParse(t *testing.T) {
	// For each secret / token flag, an empty flag value falls back to its CICADO_*
	// env var (trimmed), and a non-empty flag value wins, used exactly as typed.
	cases := []struct {
		name   string
		key    string
		envVal string
		want   string
	}{
		{"apitoken", "CICADO_API_TOKEN", "\tsentinel-apitk \t", "sentinel-apitk"},
		{"webhook", "CICADO_WEBHOOK_SECRET", "  whsec     ", "whsec"},
		{"seckey", "CICADO_SECRET_KEY", "  0123456789abcdef0123456789abcdef  ", "0123456789abcdef0123456789abcdef"},
		{"workflow", "CICADO_GITHUB_WORKFLOW_TOKEN", "\t  ghp_ws    ", "ghp_ws"},
		{"apptoken", "CICADO_GITHUB_APP_TOKEN", "ghs_app", "ghs_app"},
		{"database", "CICADO_DATABASE_URL", "postgres://u:p@h:5432/d", "postgres://u:p@h:5432/d"},
	}
	for _, tc := range cases {
		t.Run("envfallback/"+tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.envVal)
			// This is exactly what run() does: *flag = envSecret(*flag, key).
			got := envSecret("", tc.key)
			if got != tc.want {
				t.Errorf("env fallback for %s: got %q, want %q", tc.key, got, tc.want)
			}
		})
		t.Run("whitespaceenv_is_empty/"+tc.name, func(t *testing.T) {
			t.Setenv(tc.key, "     \t\n ")
			if got := envSecret("", tc.key); got != "" {
				t.Errorf("whitespace-only %s should resolve to empty (fail closed): got %q", tc.key, got)
			}
		})
	}

	// The flag value wins over any env var, used exactly as typed (interior spaces
	// preserved -- envSecret trims only the empty/whitespace fallback path, so a
	// multi-word flag value is not collapsed by the fallback helper).
	keys := []string{
		"CICADO_API_TOKEN", "CICADO_WEBHOOK_SECRET", "CICADO_SECRET_KEY",
		"CICADO_GITHUB_WORKFLOW_TOKEN", "CICADO_GITHUB_APP_TOKEN", "CICADO_DATABASE_URL",
	}
	for _, key := range keys {
		t.Run("flagwins/"+key, func(t *testing.T) {
			t.Setenv(key, "SHOULD_BE_SHADOWED")
			// N2 fix: use a value WITH interior spaces to actually exercise the
			// "interior spaces preserved on win" invariant.
			const win = "value with interior spaces"
			if got := envSecret(win, key); got != win {
				t.Errorf("flag value should win over %s and be verbatim: got %q", key, got)
			}
		})
	}
}

// TestResolveSecretFlags is B1's real guard: it exercises the SINGLE function
// run() calls after flag.Parse() -- resolveSecretFlags(f) -- on a fresh FlagSet
// with all six CICADO_* vars set to whitespace-padded sentinels, then asserts
// every field of f is the expected trimmed value. A missing assignment inside
// resolveSecretFlags leaves that field empty and fails this test.
// TestDBEnvFallbackIsWiredIntoRun guards the call being dropped from run().
func TestResolveSecretFlags(t *testing.T) {
	set := map[string]string{
		"CICADO_API_TOKEN":             "   apitk-111   ",
		"CICADO_WEBHOOK_SECRET":        "\twhsec-222\t",
		"CICADO_SECRET_KEY":            " 0123456789abcdef0123456789abcdef  ",
		"CICADO_GITHUB_WORKFLOW_TOKEN": " ghp-wf-333 ",
		"CICADO_GITHUB_APP_TOKEN":      "\tghs-app-444\t",
		"CICADO_DATABASE_URL":          " postgres://u:p@h:5432/d ",
	}
	for k, v := range set {
		t.Setenv(k, v)
	}

	fs := flag.NewFlagSet("cicado-server", flag.ContinueOnError)
	f := registerFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse(nil): %v", err)
	}

	resolveSecretFlags(f)

	// Each field must be the trimmed sentinel -- a missing assignment inside
	// resolveSecretFlags leaves the field empty and fails its check.
	if got := *f.apiToken; got != "apitk-111" {
		t.Errorf("apiToken: got %q, want %q", got, "apitk-111")
	}
	if got := *f.databaseURL; got != "postgres://u:p@h:5432/d" {
		t.Errorf("databaseURL: got %q, want %q", got, "postgres://u:p@h:5432/d")
	}
	if got := *f.webhookSecret; got != "whsec-222" {
		t.Errorf("webhookSecret: got %q, want %q", got, "whsec-222")
	}
	if got := *f.secretKeyHex; got != "0123456789abcdef0123456789abcdef" {
		t.Errorf("secretKeyHex: got %q, want %q", got, "0123456789abcdef0123456789abcdef")
	}
	if got := *f.githubWorkflowToken; got != "ghp-wf-333" {
		t.Errorf("githubWorkflowToken: got %q, want %q", got, "ghp-wf-333")
	}
	if got := *f.githubAppToken; got != "ghs-app-444" {
		t.Errorf("githubAppToken: got %q, want %q", got, "ghs-app-444")
	}

	// Flag-wins: a non-empty flag value on a fresh FS wins verbatim. This
	// verifies the "flag used as typed" invariant for the same path run() uses.
	fs2 := flag.NewFlagSet("cicado-server-2", flag.ContinueOnError)
	f2 := registerFlags(fs2)
	t.Setenv("CICADO_API_TOKEN", "from-env-9999") // should be shadowed
	if err := fs2.Parse([]string{"--api-token", "value with interior spaces"}); err != nil {
		t.Fatalf("fs2 parse: %v", err)
	}
	resolveSecretFlags(f2)
	if got := *f2.apiToken; got != "value with interior spaces" {
		t.Errorf("flag-value win: got %q, want %q (env should be shadowed)",
			got, "value with interior spaces")
	}
}

// TestDBEnvFallbackIsWiredIntoRun is a binary-level proof that run() actually makes
// the resolveSecretFlags(f) call after flag.Parse(). With a (falsely) reachable DSN
// supplied via CICADO_DATABASE_URL and NO --database-url flag, the missing-DB start-up
// error ("no database configured") must be ABSENT -- run() consumed the env fallback
// and instead failed later at the DB connect. Dropping run()'s call to
// resolveSecretFlags leaves all six flags empty, so the missing-DB error fires
// regardless of the other five, and this test fails. N1 fix: the webhook and API
// guards run before the DB connect (a dropped --webhook-secret / --api-token is
// observable at start-up via those guards); the individual-assignment guard is
// TestResolveSecretFlags at the unit level; this test pins the run()-level call
// path on its most observable field: the database URL.
func TestDBEnvFallbackIsWiredIntoRun(t *testing.T) {
	bin := buildSecretServerBinary(t, "cicado-server-db-wire-*")
	// A DSN that fails to connect immediately (localhost port 1 is closed), so the
	// binary exits fast rather than hanging on a real Postgres.
	const badDSN = "postgres://u:p@127.0.0.1:1/cicado?connect_timeout=1"

	// run starts the binary with extra env entries and returns its combined output
	// (a bounded context guards against a hang; connect-refused fails immediately).
	run := func(extra ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin,
			"--http-addr", "127.0.0.1:0",
			"--grpc-addr", "127.0.0.1:0",
			"--insecure-webhook", "--webhook-secret", "ws",
			"--insecure-api",
			"--migrate=false",
		)
		cmd.Env = append(os.Environ(), extra...)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		// The binary exits non-zero (connect refused, or missing-DB when the fallback
		// is absent); that is expected.
		_ = cmd.Run()
		return buf.String()
	}

	// Positive: the DSN arrives only via the env var. If run() does not consume the
	// fallback, *databaseURL stays "" and the binary prints "no database configured".
	if out := run("CICADO_DATABASE_URL=" + badDSN); bytes.Contains([]byte(out), []byte("no database configured")) {
		t.Fatalf("run() did not consume the CICADO_DATABASE_URL env fallback: the "+
			"'no database configured' error fired, meaning *databaseURL stayed empty.\nOutput:\n%s", out)
	}

	// Negative control: without the env var, the same invocation MUST print the
	// "no database configured" error, confirming the check above is meaningful.
	if out := run(); !bytes.Contains([]byte(out), []byte("no database configured")) {
		t.Fatalf("negative control failed: without CICADO_DATABASE_URL the binary "+
			"should print 'no database configured' but did not.\nOutput:\n%s", out)
	}
}

// buildSecretServerBinary compiles the cicado-server binary into a temp dir and
// registers cleanup.
func buildSecretServerBinary(t *testing.T, pattern string) string {
	t.Helper()
	out, err := os.CreateTemp("", pattern)
	if err != nil {
		t.Fatalf("create temp binary path: %v", err)
	}
	bin := out.Name()
	out.Close()
	t.Cleanup(func() { os.RemoveAll(bin) })
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	// --trimpath is NOT used so we match the local build closely.
	buildC := exec.CommandContext(ctx, "go", "build", "-o", bin, "cicadoci.com/cicado/server/cmd/cicado-server")
	buildC.Env = append(os.Environ(), "CGO_ENABLED=0")
	if berr := buildC.Run(); berr != nil {
		t.Fatalf("build cicado-server for leak test: %v", berr)
	}
	return bin
}

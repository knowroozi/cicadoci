package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

// TestWebhookStartupGuardFailsClosedOnEmptySecret is the operational half of
// CIC-80: a production server with no webhook signing secret must refuse to
// start unless the operator has explicitly selected the development-only
// fail-open path.
func TestWebhookStartupGuardFailsClosedOnEmptySecret(t *testing.T) {
	cases := []struct {
		name     string
		secret   string
		insecure bool
		wantErr  bool
	}{
		{name: "secret configured", secret: "whsec_abc123", insecure: false, wantErr: false},
		{name: "no secret, no opt in (fail closed)", secret: "", insecure: false, wantErr: true},
		{name: "no secret, dev opt in", secret: "", insecure: true, wantErr: false},
		{name: "both set, insecure flag accepted", secret: "s", insecure: true, wantErr: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := webhookStartupGuard(c.secret, c.insecure)
			if (err != nil) != c.wantErr {
				t.Errorf("webhookStartupGuard(%q, %v) err=%v, wantErr=%v", c.secret, c.insecure, err, c.wantErr)
			}
		})
	}
}

// TestAPIStartupGuardFailsClosedOnMissingToken is the operational half of
// CIC-150 (B2): a production server with no API token must refuse to start
// unless the operator has explicitly selected the development-only fail-open
// path via --insecure-api. This is the fail-closed guard itself, so a
// regression in it must fail a test, mirroring the webhook guard's test.
//
// The case table is the exact one the review requested.
func TestAPIStartupGuardFailsClosedOnMissingToken(t *testing.T) {
	cases := []struct {
		name     string
		token    string
		insecure bool
		wantErr  bool
	}{
		{name: "token configured", token: "secret-token", insecure: false, wantErr: false},
		{name: "no token, no opt in (fail closed)", token: "", insecure: false, wantErr: true},
		{name: "no token, dev opt in", token: "", insecure: true, wantErr: false},
		{name: "both set, insecure flag accepted", token: "t", insecure: true, wantErr: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := apiStartupGuard(c.token, c.insecure)
			if (err != nil) != c.wantErr {
				t.Errorf("apiStartupGuard(%q, %v) err=%v, wantErr=%v", c.token, c.insecure, err, c.wantErr)
			}
		})
	}
}

// TestAPITokenNotPrintedInFlagUsage is the CIC-150 B1 regression: a flag whose
// DEFAULT is the environment value prints that value in the flag usage text
// (--help, or on any mistyped flag), so the secret would land on stderr and in
// container logs. The api-token flag's default must stay empty; the secret is
// read from CICADO_API_TOKEN only AFTER flag.Parse(), mirroring main.
//
// This registers the api-token flag the way main does (empty default) and
// asserts a value placed in CICADO_API_TOKEN never appears in PrintDefaults
// output, while confirming the post-parse fallback still supplies the token.
func TestAPITokenNotPrintedInFlagUsage(t *testing.T) {
	const sentinel = "LEAKME-api-token-secret"
	t.Setenv("CICADO_API_TOKEN", sentinel)

	// The REAL registration run() uses, on a fresh set, not a hand-built stub.
	// If a revert set the api-token flag's default to os.Getenv("CICADO_API_TOKEN")
	// in registerFlags, the sentinel would be the default and PrintDefaults below
	// would print it, failing this test.
	fs := flag.NewFlagSet("cicado-server", flag.ContinueOnError)
	f := registerFlags(fs)
	if f.apiToken == nil {
		t.Fatal("registerFlags did not register --api-token")
	}

	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Before any env fallback the flag carries its registered default (""). The
	// sentinel must not be in the usage text.
	var out bytes.Buffer
	fs.SetOutput(&out)
	fs.PrintDefaults()
	if got := out.String(); strings.Contains(got, sentinel) {
		t.Fatalf("api-token secret leaked through flag usage output:\n%s", got)
	}

	// Behavior is preserved: after parse run() fills the token via the same
	// resolveAPIToken the binary uses. A flag left at its empty default falls back
	// to CICADO_API_TOKEN.
	if got := resolveAPIToken(*f.apiToken); got != sentinel {
		t.Fatalf("expected CICADO_API_TOKEN to supply the token after parse, got %q", got)
	}
}

// TestAPIStartupGuardWhitespaceFailsClosed pins CIC-150 B3/B4: a whitespace-only
// CICADO_API_TOKEN (e.g. a Secret holding only a newline) must be treated as empty
// and refused by apiStartupGuard, not start the server with an authenticator whose
// stored token is the empty string. The test routes through resolveAPIToken -- the
// SAME function run() uses -- so a trim regressing on run's path is caught here
// (Dwight's round-3 observation: trimming in-test left run's trim untested).
func TestAPIStartupGuardWhitespaceFailsClosed(t *testing.T) {
	// resolveViaEnv simulates the flag left at its empty default and forces the
	// CICADO_* fallback + trim path that resolveAPIToken owns. t.Setenv is not
	// parallel-safe, so this test is sequential.
	resolveViaEnv := func(rawEnv string) string {
		t.Setenv("CICADO_API_TOKEN", rawEnv)
		return resolveAPIToken("") // empty flag value -> falls back to the env var
	}

	for _, v := range []string{"\n", "  ", "\t", " \u00a0 "} {
		if got := resolveViaEnv(v); got != "" {
			t.Fatalf("resolveAPIToken did not trim whitespace-only token %q to empty; got %q", v, got)
		}
		if err := apiStartupGuard(resolveViaEnv(v), false); err == nil {
			t.Fatalf("whitespace-only token %q: guard failed open; expected refusal to start", v)
		}
		// With the explicit dev opt-in the guard passes even for an empty token.
		if err := apiStartupGuard(resolveViaEnv(v), true); err != nil {
			t.Fatalf("--insecure-api should pass the guard even for an empty token, got %v", err)
		}
	}

	// A real flag-set token is accepted as-is (no fallback, no trim of interior).
	if err := apiStartupGuard(resolveAPIToken("real-token"), false); err != nil {
		t.Fatalf("non-empty flag token rejected: %v", err)
	}

	// Fully empty CICADO_API_TOKEN + no opt-in fails closed; with the opt-in it passes.
	if err := apiStartupGuard(resolveViaEnv(""), false); err == nil {
		t.Fatalf("empty CICADO_API_TOKEN without --insecure-api should fail closed")
	}
	if err := apiStartupGuard(resolveViaEnv(""), true); err != nil {
		t.Fatalf("empty CICADO_API_TOKEN with --insecure-api should pass (dev path), got %v", err)
	}
}

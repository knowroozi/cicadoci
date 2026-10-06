package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"cicadoci.com/cicado/server/api"
	"cicadoci.com/cicado/server/approvalgates"
	"cicadoci.com/cicado/server/audit"
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

// TestResolveApprovalTimeout is the table test for the CIC-160 F1 last-fix:
// the env var path must refuse startup on unparseable or negative values,
// not silently fall back to "never time out".
func TestResolveApprovalTimeout(t *testing.T) {
	tCases := []struct {
		name     string
		flagVal  time.Duration
		override string
		want     time.Duration
		wantErr  bool
	}{
		{"valid env override", 0, "1h30m", time.Hour + 30*time.Minute, false},
		{"bare number in env", 0, "24", 0, true},
		{"unparseable in env", 0, "abc", 0, true},
		{"negative in env", 0, "-1h", 0, true},
		{"negative flag", -time.Hour, "", 0, true},
		{"zero flag", 0, "", 0, false},
		{"nonzero flag, no env", time.Hour, "", time.Hour, false},
		{"env overrides flag", time.Hour, "30m", 30 * time.Minute, false},
	}
	for _, tc := range tCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveApprovalTimeout(tc.flagVal, tc.override)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveApprovalTimeout(%d, %q): wanted error, got nil (result=%v)",
						tc.flagVal, tc.override, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveApprovalTimeout(%d, %q): unexpected error: %v",
					tc.flagVal, tc.override, err)
			}
			if got != tc.want {
				t.Errorf("resolveApprovalTimeout(%d, %q): got %v, want %v",
					tc.flagVal, tc.override, got, tc.want)
			}
		})
	}
}

// TestApprovalgatesFlagsAreWired registers the real flag set on a fresh
// FlagSet and verifies the CIC-162 flags are present with fail-closed
// defaults. The channel and timeout are CIC-160's --approval-channel /
// --approval-timeout; there is no separate approvalgates channel flag.
func TestApprovalgatesFlagsAreWired(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	f := registerFlags(fs)

	if *f.approvalgatesEnabled {
		t.Errorf("--approvalgates-enabled default = true, want false")
	}
	if *f.approvalgatesAllowed != "" {
		t.Errorf("--approvalgates-allowed default = %q, want \"\" (fail closed)", *f.approvalgatesAllowed)
	}
	if *f.insecureApprovalgatesNoop {
		t.Errorf("--insecure-approvalgates-noop default = true, want false")
	}
	if fs.Lookup("approvalgates-channel") != nil {
		t.Errorf("--approvalgates-channel is registered; the channel must come from --approval-channel (CIC-160)")
	}
}

func TestSplitAllowList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{" , ,", nil},
		{"U1", []string{"U1"}},
		{" U1 ,U2,, U3 ", []string{"U1", "U2", "U3"}},
	}
	for _, tc := range cases {
		got := splitAllowList(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitAllowList(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestBuildApprovalGates(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("disabled builds nothing", func(t *testing.T) {
		mgr, approvers, _, err := buildApprovalGates(ctx, false, true, "U1", audit.NewMemoryLog(), log)
		if err != nil || mgr != nil || approvers != nil {
			t.Fatalf("got (%v, %v, %v), want (nil, nil, nil)", mgr, approvers, err)
		}
	})
	t.Run("enabled without noop ack refuses startup", func(t *testing.T) {
		mgr, _, _, err := buildApprovalGates(ctx, true, false, "U1", audit.NewMemoryLog(), log)
		if err == nil || mgr != nil {
			t.Fatalf("got (%v, %v), want (nil, error)", mgr, err)
		}
	})
	t.Run("enabled with noop ack builds manager and parses allow-list", func(t *testing.T) {
		mgr, approvers, _, err := buildApprovalGates(ctx, true, true, " U1, ,U2 ", audit.NewMemoryLog(), log)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mgr == nil {
			t.Fatal("manager is nil")
		}
		if want := []string{"U1", "U2"}; !reflect.DeepEqual(approvers, want) {
			t.Errorf("approvers = %#v, want %#v", approvers, want)
		}
	})
	t.Run("resolution writes an audit row with the outcome action and actor", func(t *testing.T) {
		al := audit.NewMemoryLog()
		mgr, approvers, waitGateAudit, err := buildApprovalGates(ctx, true, true, "U1", al, log)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if err := mgr.Open(ctx, &approvalgates.Gate{RunID: "run-1", Channel: "C1", Allow: "approve", Deny: "deny", Allowed: approvers}); err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := mgr.Resolve(ctx, "run-1", "U1", "deny"); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		// CIC-193 G1: onResolved now dispatches the audit write asynchronously and
		// the wiring joins it; drain before asserting the row exists.
		waitGateAudit()
		entries, err := al.List(ctx, 0, 0)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(entries) == 0 {
			t.Fatal("no audit entry written")
		}
		last := entries[len(entries)-1]
		if last.Action != audit.ActionDeny || last.Actor != "U1" || last.Target != "run-1" {
			t.Errorf("audit entry = %+v, want action=deny actor=U1 target=run-1", last)
		}
	})
}

// TestGateAuditEntry pins the state -> audit action mapping (#135 B2): only an
// approved gate is recorded as ActionApprove.
func TestGateAuditEntry(t *testing.T) {
	cases := []struct {
		state     approvalgates.GateState
		actorID   string
		wantAct   audit.Action
		wantActor string
	}{
		{approvalgates.GateApproved, "U1", audit.ActionApprove, "U1"},
		{approvalgates.GateDenied, "U2", audit.ActionDeny, "U2"},
		{approvalgates.GateTimedOut, "", audit.ActionGateTimeout, "system"},
		{approvalgates.GateTimedOut, "U3", audit.ActionGateTimeout, "system"},
	}
	for _, tc := range cases {
		e := gateAuditEntry("run-1", tc.state, tc.actorID)
		if e.Action != tc.wantAct || e.Actor != tc.wantActor || e.Target != "run-1" {
			t.Errorf("gateAuditEntry(%v, %q) = %+v, want action=%s actor=%s", tc.state, tc.actorID, e, tc.wantAct, tc.wantActor)
		}
		if e.Reason != "gate "+tc.state.String() {
			t.Errorf("gateAuditEntry(%v).Reason = %q", tc.state, e.Reason)
		}
	}
}

// TestApiAuthOpts pins CIC-194 S1: the operator write surface is armed ONLY
// when a token is present, so an --insecure-api (tokenless) server exposes no
// write routes at all, whereas a token present adds exactly the two options
// (WithWriteDeps + WithAPIToken), and in that order. Extracted from run() as
// apiAuthOpts (R4) so the two branches are table-tested and cannot drift.
func TestApiAuthOpts(t *testing.T) {
	cases := []struct {
		name    string
		token   string
		deps    api.WriteDeps
		wantLen int
	}{
		{name: "no token -> no options (insecure-api: no write surface)", token: "", wantLen: 0},
		{name: "token -> write deps + token options", token: "secret-token", deps: api.WriteDeps{}, wantLen: 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := apiAuthOpts(c.token, c.deps)
			if len(got) != c.wantLen {
				t.Fatalf("apiAuthOpts(%q) returned %d options, want %d", c.token, len(got), c.wantLen)
			}
			// A token path must arm BOTH the write-deps route and its bearer
			// gate -- a write route without the token would be an open operator
			// surface, the exact hole S1 closes.
			if c.wantLen > 0 && len(got) != 2 {
				t.Errorf("token path expected exactly 2 options (write deps + token), got %d", len(got))
			}
		})
	}
}

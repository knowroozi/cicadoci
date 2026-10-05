package main

import (
	"testing"
)

// TestEnvInt64Or_ValidParsesEnvVar confirms that when the flag value is 0,
// a valid CICADO_GITHUB_APP_ID env var is parsed and returned.
func TestEnvInt64Or_ValidParsesEnvVar(t *testing.T) {
	t.Setenv("CICADO_GITHUB_APP_ID", "12345")

	got, err := envInt64Or(0, "CICADO_GITHUB_APP_ID")
	if err != nil {
		t.Fatalf("envInt64Or(0) err = %v", err)
	}
	if got != 12345 {
		t.Fatalf("envInt64Or = %d, want 12345", got)
	}
}

// TestEnvInt64Or_FlagWins confirms that a non-zero flag wins over the env var.
func TestEnvInt64Or_FlagWins(t *testing.T) {
	t.Setenv("CICADO_GITHUB_APP_ID", "12345")

	got, err := envInt64Or(999, "CICADO_GITHUB_APP_ID")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != 999 {
		t.Fatalf("flag wins: got %d, want 999", got)
	}
}

// TestEnvInt64Or_BadValueFails confirms a bad env value returns an error.
func TestEnvInt64Or_BadValueFails(t *testing.T) {
	t.Setenv("CICADO_GITHUB_APP_ID", "notanumber")

	_, err := envInt64Or(0, "CICADO_GITHUB_APP_ID")
	if err == nil {
		t.Fatal("expected error for bad env value")
	}
}

// TestEnvInt64Or_NegativeFails confirms a negative env value is rejected.
func TestEnvInt64Or_NegativeFails(t *testing.T) {
	t.Setenv("CICADO_GITHUB_APP_ID", "-1")

	_, err := envInt64Or(0, "CICADO_GITHUB_APP_ID")
	if err == nil {
		t.Fatal("expected error for negative env value")
	}
}

// TestEnvInt64Or_ZeroDefaultWhenNoEnv confirms that with neither the flag
// nor the env var set, the result is 0.
func TestEnvInt64Or_ZeroDefaultWhenNoEnv(t *testing.T) {
	t.Setenv("CICADO_GITHUB_APP_ID", "")

	got, err := envInt64Or(0, "CICADO_GITHUB_APP_ID")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}

// TestEnvSecret_KeyFileEnvFallback verifies that envSecret resolves
// CICADO_GITHUB_APP_KEY_FILE for the --github-app-key-file flag.
func TestEnvSecret_KeyFileEnvFallback(t *testing.T) {
	t.Setenv("CICADO_GITHUB_APP_KEY_FILE", "   /mnt/secrets/app_key.pem   ")

	got := envSecret("", "CICADO_GITHUB_APP_KEY_FILE")
	want := "/mnt/secrets/app_key.pem"
	if got != want {
		t.Fatalf("envSecret trimmed = %q, want %q", got, want)
	}
}

// TestEnvSecret_KeyFileFlagWins confirms the flag wins over the env var.
func TestEnvSecret_KeyFileFlagWins(t *testing.T) {
	t.Setenv("CICADO_GITHUB_APP_KEY_FILE", "/from/env")

	got := envSecret("/from/flag", "CICADO_GITHUB_APP_KEY_FILE")
	if got != "/from/flag" {
		t.Fatalf("flag-wins: got %q, want /from/flag", got)
	}
}

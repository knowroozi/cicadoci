package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestParseVersionForms(t *testing.T) {
	tests := []struct {
		in     string
		want   Ver
		wantOK bool
	}{
		{"1.26", Ver{major: 1, minor: 26}, true},
		{"1.26.0", Ver{major: 1, minor: 26}, true},
		{"1.22.5", Ver{major: 1, minor: 22, patch: 5}, true},
		{"1.26.0+build", Ver{major: 1, minor: 26}, true}, // build suffix ignored
		{"bullseye", Ver{}, false},                       // distro tag, not a version
		{"latest", Ver{}, false},
		{"", Ver{}, false},
	}
	for _, tt := range tests {
		got, err := parseVersion(tt.in)
		if tt.wantOK {
			if err != nil {
				t.Errorf("parseVersion(%q) unexpected err %v", tt.in, err)
				continue
			}
			if got != tt.want {
				t.Errorf("parseVersion(%q) = %v, want %v", tt.in, got, tt.want)
			}
		} else if err == nil {
			t.Errorf("parseVersion(%q) expected error, got %v", tt.in, got)
		}
	}
}

func TestLessThan(t *testing.T) {
	if !(Ver{major: 1, minor: 23}).lessThan(Ver{major: 1, minor: 26}) {
		t.Error("1.23 should be < 1.26")
	}
	if (Ver{major: 1, minor: 26}).lessThan(Ver{major: 1, minor: 26}) {
		t.Error("1.26 should not be < 1.26")
	}
	if !(Ver{major: 1, minor: 26}).lessThan(Ver{major: 1, minor: 26, patch: 5}) {
		t.Error("1.26 (==1.26.0) should be < 1.26.5")
	}
	if !(Ver{major: 1, minor: 26, patch: 3}).lessThan(Ver{major: 1, minor: 27}) {
		t.Error("1.26.3 should be < 1.27")
	}
}

func TestCheckPassOnMatchingVersions(t *testing.T) {
	// Hermetic equivalent of "the real repo": go 1.26.0 vs golang:1.26 passes
	// (patch omitted on the base == 1.26.0).
	goMod := writeTemp(t, "go.mod", "module x\n\ngo 1.26.0\n")
	df := writeTemp(t, "Dockerfile", "FROM golang:1.26 AS build\nFROM debian:12-slim AS runner\n")
	if err := Check(goMod, df); err != nil {
		t.Fatalf("1.26.0 vs golang:1.26 should pass, got %v", err)
	}
}

func TestCheckRequiresDirective(t *testing.T) {
	// A go.mod without a `go` directive is an unverifiable state.
	goMod := writeTemp(t, "go.mod", "module x\n\nrequire (\n\texample.com/a v1.0.0\n)\n")
	df := writeTemp(t, "Dockerfile", "FROM golang:1.26 AS build\n")
	if err := Check(goMod, df); err == nil {
		t.Fatal("expected failure: go.mod has no `go` directive")
	}
}

func TestCheckFailsOnStaleBase(t *testing.T) {
	goMod := writeTemp(t, "go.mod", "module x\n\ngo 1.26.0\n")
	df := writeTemp(t, "Dockerfile", "FROM golang:1.23 AS build\nFROM debian:12-slim AS runner\n")
	if err := Check(goMod, df); err == nil {
		t.Fatal("expected failure: golang:1.23 < go 1.26.0")
	}
}

func TestCheckMultipleBasesOneStale(t *testing.T) {
	goMod := writeTemp(t, "go.mod", "module x\n\ngo 1.26.0\n")
	df := writeTemp(t, "Dockerfile",
		"FROM golang:1.26 AS build\n"+
			"FROM golang:1.22 AS older\n"+
			"FROM debian:12-slim AS runtime\n")
	if err := Check(goMod, df); err == nil {
		t.Fatal("expected failure: a 1.22 base is stale even though 1.26 is fine")
	}
}

func TestCheckNoGolangBase(t *testing.T) {
	goMod := writeTemp(t, "go.mod", "module x\n\ngo 1.26.0\n")
	df := writeTemp(t, "Dockerfile", "FROM debian:12-slim AS app\n")
	if err := Check(goMod, df); err == nil {
		t.Fatal("expected failure: no golang: base to verify")
	}
}

func TestCheckIgnoresCommentedAndCaseInsensitive(t *testing.T) {
	goMod := writeTemp(t, "go.mod", "module x\n\ngo 1.26.0\n")
	df := writeTemp(t, "Dockerfile",
		"# FROM golang:1.20 AS build   <- commented, must be ignored\n"+
			"  from golang:1.26 as build\n") // indented, lowercase FROM
	if err := Check(goMod, df); err != nil {
		t.Fatalf("expected pass (commented base ignored; FROM indented/lowercase), got %v", err)
	}
}

// TestCheckToleratesPlatformFlag guards against the exact bypass this ticket exists to
// prevent: a `FROM --platform=... golang:X` line must still be parsed for its version.
func TestCheckToleratesPlatformFlagMismatch(t *testing.T) {
	goMod := writeTemp(t, "go.mod", "module x\n\ngo 1.26.0\n")
	// Multi-arch build form: platform flag between FROM and the base image. The old
	// anchored regex `^\s*FROM\s+golang:` did NOT match this, so the guard would
	// silently pass -- precisely the "silently bypassable by a reasonable future edit"
	// case the ticket calls out. The fixed regex must detect the 1.23 stale base.
	df := writeTemp(t, "Dockerfile",
		"FROM --platform=$BUILDPLATFORM golang:1.23 AS build\n"+
			"FROM debian:12-slim AS runtime\n")
	if err := Check(goMod, df); err == nil {
		t.Fatal("expected failure: FROM --platform=... golang:1.23 < go 1.26.0 must be detected")
	}
}

func TestCheckToleratesPlatformFlagMatch(t *testing.T) {
	goMod := writeTemp(t, "go.mod", "module x\n\ngo 1.24\n")
	df := writeTemp(t, "Dockerfile",
		"FROM --platform=linux/amd64 golang:1.24-bookworm AS build\n")
	if err := Check(goMod, df); err != nil {
		t.Fatalf("expected pass (1.24 vs golang:1.24-bookworm via platform flag), got %v", err)
	}
}

func TestCheckMissingFile(t *testing.T) {
	if err := Check("/no/such/go.mod", "Dockerfile"); err == nil {
		t.Fatal("expected error for missing go.mod")
	}
}

func TestCheckIgnoresGoModRequireLine(t *testing.T) {
	// A `require (go-something ...)` line must not be mistaken for the `go` directive.
	goMod := writeTemp(t, "go.mod", "module x\n\ngo 1.26.0\n\nrequire (\n\tgithub.com/foo/go-tool v1.2.0\n)\n")
	df := writeTemp(t, "Dockerfile", "FROM golang:1.26 AS build\n")
	if err := Check(goMod, df); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
}

package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/backpressure"
	"cicadoci.com/cicado/server/forge"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/webhook"
)

// discardLogger is a slog.Logger that discards all output.
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// ── stub ─────────────────────────────────────────────────────────────────────────

type stubForge struct {
	prepareCalls int
	prepareErr   error
}

func (s *stubForge) Name() string { return "stub" }

func (s *stubForge) Prepare(_ context.Context, _ forge.RunRef) (int64, error) {
	s.prepareCalls++
	return int64(s.prepareCalls), s.prepareErr
}

func (s *stubForge) Start(_ context.Context, _ int64, _ forge.RunRef) error { return nil }

func (s *stubForge) Complete(_ context.Context, _ int64, _ forge.RunRef,
	_ forge.Conclusion, _ []forge.Annotation) error {
	return nil
}

func (s *stubForge) Rerun(_ context.Context, _ forge.RerunRef) (string, error) {
	return "", nil
}

func (s *stubForge) CloneCredentialsForRun(_ context.Context, _ forge.RunRef) (string, error) {
	return "", nil
}

var _ forge.Forge = (*stubForge)(nil)

// ── tests ─────────────────────────────────────────────────────────────────────────

func TestSplitRepo(t *testing.T) {
	cases := []struct {
		fullName string
		owner    string
		repo     string
		wantOk   bool
	}{
		{"acme/cicadoci", "acme", "cicadoci", true},
		{"owner/repo", "owner", "repo", true},
		{"", "", "", false},
		{"noslash", "", "", false},
		{"/leading", "", "", false},
	}
	for _, c := range cases {
		got, got2, ok := splitRepo(c.fullName)
		if ok != c.wantOk {
			t.Errorf("splitRepo(%q) ok=%v, want %v", c.fullName, ok, c.wantOk)
		}
		if ok && (got != c.owner || got2 != c.repo) {
			t.Errorf("splitRepo(%q) = %q,%q; want %q,%q", c.fullName, got, got2, c.owner, c.repo)
		}
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Backpressure == nil {
		t.Fatal("DefaultConfig: Backpressure is nil")
	}
	allowed, err := cfg.Backpressure.Acquire(t.Context(), "repo1", "org1")
	if err != nil {
		t.Fatal("DefaultConfig: acquire failed:", err)
	}
	if !allowed {
		t.Fatal("DefaultConfig: default limits should allow first acquire")
	}
}

func TestReport(t *testing.T) {
	f := &stubForge{}
	report(t.Context(), f, "acme", "cicadoci", "deadbeef", uuid.New(), "main", discardLogger)
	if f.prepareCalls != 1 {
		t.Fatalf("report: prepareCalls = %d, want 1", f.prepareCalls)
	}
}

func TestReport_Failure(t *testing.T) {
	f := &stubForge{prepareErr: errors.New("boom")}
	// report swallows the error (logs it); it doesn't return.
	report(t.Context(), f, "acme", "cicadoci", "deadbeef", uuid.New(), "main", discardLogger)
	if f.prepareCalls != 1 {
		t.Fatalf("report_with_err: expected 1 call, got %d", f.prepareCalls)
	}
}

func TestNew(t *testing.T) {
	c := New(nil, nil, Config{}, nil)
	if c == nil {
		t.Fatal("New returned nil controller")
	}
	if c.log == nil {
		t.Fatal("New(nil logger): c.log should default to slog.Default(), not nil")
	}
}

func TestNew_ImplementsDispatcher(t *testing.T) {
	c := New(nil, nil, Config{}, nil)
	var _ webhook.Dispatcher = c
}

// ── compile-time checks ───────────────────────────────────────────────────────────

var (
	_ = store.CreateRunOptions{}
	_ = backpressure.Tracker{}
	_ = webhook.PushPayload{}
	_ = time.Second
	_ = forge.ConclusionSuccess
)

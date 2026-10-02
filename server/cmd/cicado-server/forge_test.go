package main

import (
	"io"
	"log/slog"
	"testing"

	"cicadoci.com/cicado/server/controller"
)

// CIC-81: a configured token selects the GitHub Checks forge; no token keeps
// the NoOp forge, so a dev server never calls GitHub.
func TestNewForge(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, ok := newForge("", log).(controller.NoOpForge); !ok {
		t.Error("newForge(\"\") is not the NoOp forge")
	}
	if got := newForge("ghs_token", log).Name(); got != "github" {
		t.Errorf("newForge(token).Name() = %q, want github", got)
	}
}

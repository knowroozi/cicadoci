package lambda

import (
	"context"
	"log/slog"
	"testing"

	"cicadoci.com/cicado/runner/journal"
)

func TestNew(t *testing.T) {
	h := New(nil, nil, slog.Default())
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestDispatch(t *testing.T) {
	h := New(nil, journal.NewMemoryJournal(), slog.Default())
	resp, err := h.Dispatch(context.Background(), &JobEvent{
		RunID:      "run-123",
		JobID:      "job-456",
		Attempt:    1,
		LeaseToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", resp.ExitCode)
	}
}

func TestJobEventStruct(t *testing.T) {
	evt := JobEvent{
		RunID:      "run-123",
		JobID:      "job-456",
		Attempt:    1,
		LeaseToken: "token",
	}
	if evt.RunID != "run-123" {
		t.Error("wrong RunID")
	}
	if evt.JobID != "job-456" {
		t.Error("wrong JobID")
	}
}

func TestJobResponseStruct(t *testing.T) {
	resp := JobResponse{
		Success:      true,
		ExitCode:     0,
		FailureClass: "",
		StepLogs:     map[string]string{"step-1": "output"},
	}
	if !resp.Success {
		t.Error("expected success")
	}
}

package runstatus

import (
	"testing"

	"cicadoci.com/cicado/server/chat"
)

func TestTrackerNew(t *testing.T) {
	provider := chat.NewSlackProvider()
	tk, err := NewTracker(t.Context(), provider, "#ci", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if tk.msgID == "" {
		t.Fatal("expected non-empty message ID")
	}
	records := provider.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].Channel != "#ci" {
		t.Fatalf("expected channel #ci, got %q", records[0].Channel)
	}
}

func TestTrackerOnStep(t *testing.T) {
	provider := chat.NewSlackProvider()
	tk, err := NewTracker(t.Context(), provider, "#ci", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	err = tk.OnStep(t.Context(), StepUpdate{StepIndex: 0, StepName: "build", State: "running"})
	if err != nil {
		t.Fatal(err)
	}
	records := provider.Records()
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	if records[1].ParentID != string(tk.msgID) {
		t.Fatalf("expected parent %q, got %q", tk.msgID, records[1].ParentID)
	}
}

func TestTrackerOnFinish(t *testing.T) {
	provider := chat.NewSlackProvider()
	tk, _ := NewTracker(t.Context(), provider, "#ci", "run-1")
	// Passed
	if err := tk.OnFinish(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	// Failed
	tk2, _ := NewTracker(t.Context(), provider, "#ci", "run-2")
	if err := tk2.OnFinish(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	records := provider.Records()
	last := records[len(records)-1]
	if last.Payload.Color != "#e01e5a" {
		t.Fatalf("expected red for failed, got %q", last.Payload.Color)
	}
}

func TestTrackerStepProgress(t *testing.T) {
	provider := chat.NewSlackProvider()
	tk, _ := NewTracker(t.Context(), provider, "#ci", "run-1")
	if err := tk.OnStep(t.Context(), StepUpdate{StepIndex: 1, StepName: "test", State: "passed"}); err != nil {
		t.Fatal(err)
	}
	records := provider.Records()
	// Last record should be a thread message
	last := records[len(records)-1]
	if last.Payload.Text == "" {
		t.Fatal("expected non-empty text")
	}
}

// Ensure the type alias works.
var _ StepProgress

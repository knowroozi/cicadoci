package events

import (
	"encoding/json"
	"testing"
)

func TestValidKinds(t *testing.T) {
	for kind := range ValidKinds {
		if !IsValidKind(kind) {
			t.Errorf("IsValidKind(%q) = false, want true", kind)
		}
	}
	if IsValidKind("nonexistent") {
		t.Error("IsValidKind(\"nonexistent\") = true, want false")
	}
}

func TestEventStepStartedJSON(t *testing.T) {
	ev := EventStepStarted{StepIndex: 0, StepName: "build", Ts: "2026-01-01T00:00:00Z"}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	back := EventStepStarted{}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.StepIndex != 0 || back.StepName != "build" || back.Ts != "2026-01-01T00:00:00Z" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}

func TestEventStepFinishedJSON(t *testing.T) {
	ev := EventStepFinished{
		StepIndex:  1,
		StepName:   "test",
		ExitCode:   42,
		DurationMs: 1500,
		Ts:         "2026-01-01T00:01:00Z",
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	back := EventStepFinished{}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ExitCode != 42 || back.DurationMs != 1500 || back.StepName != "test" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}

func TestEventErrorClassifiedJSON(t *testing.T) {
	ev := EventErrorClassified{
		StepIndex:      0,
		Classification: "test_failure",
		Confidence:     0.95,
		Ts:             "2026-01-01T00:00:00Z",
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	back := EventErrorClassified{}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Classification != "test_failure" || back.Confidence != 0.95 {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}

func TestArtifactEventJSON(t *testing.T) {
	ev := ArtifactEvent{
		ArtifactName: "binary",
		BlobKey:      "acme/run-1/binary",
		SizeBytes:    12345,
		ContentType:  "application/octet-stream",
		Ts:           "2026-01-01T00:00:00Z",
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	back := ArtifactEvent{}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ArtifactName != "binary" || back.BlobKey != "acme/run-1/binary" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}

func TestLogGapEventJSON(t *testing.T) {
	ev := LogGapEvent{FromSeq: 100, ToSeq: 200, Ts: "2026-01-01T00:00:00Z"}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	back := LogGapEvent{}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.FromSeq != 100 || back.ToSeq != 200 {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}

func TestAllKindsCovered(t *testing.T) {
	kinds := []string{StepStarted, StepFinished, ErrorClassified, ArtifactUploaded, LogGap}
	if len(kinds) != len(ValidKinds) {
		t.Fatalf("ValidKinds has %d entries, expected %d", len(ValidKinds), len(kinds))
	}
	for _, k := range kinds {
		if !IsValidKind(k) {
			t.Errorf("IsValidKind(%q) = false", k)
		}
	}
}

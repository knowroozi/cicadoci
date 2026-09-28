package mcp

import (
	"context"
	"testing"
)

func TestAllTools(t *testing.T) {
	tools := AllTools()
	if len(tools) != 8 {
		t.Fatalf("expected 8 tools, got %d", len(tools))
	}
	seen := map[ToolName]bool{}
	for _, tn := range tools {
		if seen[tn] {
			t.Errorf("duplicate tool: %s", tn)
		}
		seen[tn] = true
	}
	// Verify all known tools are present.
	for _, want := range []string{
		string(ToolListRuns),
		string(ToolGetRun),
		string(ToolGetJob),
		string(ToolGetFailedStepLogs),
		string(ToolGetStructuredEvents),
		string(ToolClassifyFailure),
		string(ToolDiffGreen),
		string(ToolValidateWorkflow),
	} {
		if !seen[ToolName(want)] {
			t.Errorf("missing tool: %s", want)
		}
	}
}

func TestServerAuditCalled(t *testing.T) {
	// Verify the audit callback is invoked.
	audited := 0
	s := New(nil, func(_, _, _ string) { audited++ })
	// Call ClassifyFailure (doesn't need reader)
	_, err := s.ClassifyFailure(context.Background(), "user-1", ClassifyFailureInput{ExitCode: 137})
	if err != nil {
		t.Fatal(err)
	}
	if audited != 1 {
		t.Fatalf("expected 1 audit call, got %d", audited)
	}
}

func TestClassifyFailureOOM(t *testing.T) {
	s := New(nil, nil)
	r, err := s.ClassifyFailure(context.Background(), "user-1", ClassifyFailureInput{ExitCode: 137})
	if err != nil {
		t.Fatal(err)
	}
	// The package import test verifies this compiles.
	if r.Classification != "oom" {
		t.Fatalf("expected oom, got %q", r.Classification)
	}
}

func TestValidateWorkflowEmpty(t *testing.T) {
	s := New(nil, nil)
	// An empty workflow should fail parse validation.
	err := s.ValidateWorkflow(context.Background(), "user-1", ValidateWorkflowInput{
		YAML:   []byte(""),
		Source: "",
	})
	if err == nil {
		t.Fatal("expected error for empty YAML")
	}
}

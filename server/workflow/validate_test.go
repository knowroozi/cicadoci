package workflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// validWorkflow returns a well-formed minimal workflow for use in tests.
func validWorkflow() *Workflow {
	return &Workflow{
		Name: "test",
		On: On{
			Push: &PushTrigger{Branches: []string{"main"}},
		},
		Jobs: Jobs{
			"build": {
				RunsOn: []string{"linux"},
				Steps: []*Step{
					{Name: "checkout", Run: "true"},
					{Name: "compile", Run: "go build ./...", Image: "golang:1.24"},
				},
			},
		},
	}
}

func TestValidate_Nil(t *testing.T) {
	if err := Validate(nil); err == nil {
		t.Fatal("expected error for nil workflow")
	}
}

func TestValidate_MinimalValid(t *testing.T) {
	w := validWorkflow()
	if err := Validate(w); err != nil {
		t.Fatalf("expected valid workflow, got: %v", err)
	}
}

func TestValidate_EmptyName(t *testing.T) {
	w := validWorkflow()
	w.Name = ""
	err := Validate(w)
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if len(verr.Errors()) == 0 {
		t.Fatal("expected non-zero errors")
	}
}

func TestValidate_NoTriggers(t *testing.T) {
	w := validWorkflow()
	w.On = On{} // no triggers
	err := Validate(w)
	if err == nil {
		t.Fatal("expected error for no triggers")
	}
}

func TestValidate_NoJobs(t *testing.T) {
	w := validWorkflow()
	w.Jobs = Jobs{}
	err := Validate(w)
	if err == nil {
		t.Fatal("expected error for no jobs")
	}
}

func TestValidate_JobNoSteps(t *testing.T) {
	w := validWorkflow()
	w.Jobs["build"].Steps = nil
	err := Validate(w)
	if err == nil {
		t.Fatal("expected error for job with no steps")
	}
}

func TestValidate_StepNoUsesNoRun(t *testing.T) {
	w := validWorkflow()
	// Neither uses nor run.
	w.Jobs["build"].Steps = []*Step{{Name: "empty"}}
	err := Validate(w)
	if err == nil {
		t.Fatal("expected error for step with neither uses nor run")
	}
	if !strings.Contains(err.Error(), "'run' is required") {
		t.Errorf("error = %v, want \"'run' is required\"", err)
	}
}

func TestValidate_StepBothUsesAndRun(t *testing.T) {
	w := validWorkflow()
	w.Jobs["build"].Steps = []*Step{
		{Name: "bad", Uses: "checkout", Run: "echo hi"},
	}
	err := Validate(w)
	if err == nil {
		t.Fatal("expected error for step with both uses and run")
	}
	// CIC-147: only the mutually-exclusive error, not a second "uses not
	// supported" error for the same step.
	if msg := err.Error(); !strings.Contains(msg, "mutually exclusive") || strings.Contains(msg, "not yet supported") || strings.Contains(msg, "checkout is automatic") {
		t.Errorf("error = %v, want only the mutually-exclusive error", err)
	}
}

func TestValidate_InvalidIf(t *testing.T) {
	w := validWorkflow()
	// "always!" is not a valid if-value.
	w.Jobs["build"].Steps = []*Step{
		{Name: "bad-if", Run: "echo hi", If: "always!"},
	}
	err := Validate(w)
	if err == nil {
		t.Fatal("expected error for invalid 'if' value")
	}
}

func TestValidate_ValidIfValues(t *testing.T) {
	// Only 'success' and '' (default) are valid for step.if in the current
	// runner; 'failure' and 'always' are rejected by the validator.
	for _, v := range []string{"success", ""} {
		w := validWorkflow()
		w.Jobs["build"].Steps = []*Step{{Name: "ok", Run: "echo hi", If: v}}
		if err := Validate(w); err != nil {
			t.Errorf("if=%q: unexpected error: %v", v, err)
		}
	}
}

// TestValidate_StepIfFailureNotSupported verifies that 'if: failure' and
// 'if: always' are rejected on EVERY step (including step 0).
// The runner is not yet wired to honour step-level if: conditions
// (CIC-118); the validator fails loudly until then.
func TestValidate_StepIfFailureNotSupported(t *testing.T) {
	for _, v := range []string{"failure", "always"} {
		w := validWorkflow()
		// Single step at index 0 with the unsupported condition.
		w.Jobs["build"].Steps = []*Step{{Name: "only", Run: "echo hi", If: v}}
		err := Validate(w)
		if err == nil {
			t.Errorf("if=%q on step 0: expected error (not yet supported), got nil", v)
		}
	}
}

func TestValidate_InvalidIdentity(t *testing.T) {
	w := validWorkflow()
	w.Jobs["build"].Steps = []*Step{
		{Name: "bad-identity", Run: "echo hi", Identity: "gcp"},
	}
	err := Validate(w)
	if err == nil {
		t.Fatal("expected error for invalid 'identity' value")
	}
}

func TestValidate_ValidIdentities(t *testing.T) {
	for _, v := range []string{"", "none", "aws"} {
		w := validWorkflow()
		w.Jobs["build"].Steps = []*Step{{Name: "ok", Run: "echo hi", Identity: v}}
		if err := Validate(w); err != nil {
			t.Errorf("identity=%q: unexpected error: %v", v, err)
		}
	}
}

func TestValidate_NilJob(t *testing.T) {
	w := validWorkflow()
	w.Jobs["build"] = nil
	err := Validate(w)
	if err == nil {
		t.Fatal("expected error for nil job in map")
	}
}

func TestValidate_ApprovalGateMissingAllow(t *testing.T) {
	// An approval gate with a channel but no allow-list is invalid.
	// The JSON Schema requires allow with minItems: 1, and the Go
	// validator enforces the same constraint so the two agree.
	w := validWorkflow()
	w.Jobs["deploy"] = &Job{
		Steps:    []*Step{{Name: "deploy", Run: "aws s3 sync ./dist s3://bucket"}},
		Approval: &ApprovalGate{Channel: "#deploys"},
	}
	err := Validate(w)
	if err == nil {
		t.Fatal("expected error for approval gate with empty allow-list")
		return
	}
	verr := &ValidationError{}
	if !errors.As(err, &verr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if len(verr.Errors()) == 0 {
		t.Error("expected at least one error for empty allow-list")
	}
}

func TestValidate_ApprovalGateWithAllowList(t *testing.T) {
	// A valid approval gate with a non-empty allow-list passes.
	w := validWorkflow()
	w.Jobs["deploy"] = &Job{
		Steps:    []*Step{{Name: "deploy", Run: "aws s3 sync ./dist s3://bucket"}},
		Approval: &ApprovalGate{Channel: "#deploys", Allow: []string{"alice", "bob"}},
	}
	if err := Validate(w); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSchemaV1_NotEmpty(t *testing.T) {
	if len(SchemaV1) == 0 {
		t.Fatal("embedded schema is empty — embed directive failed")
	}
	s := SchemaV1String()
	if s == "" {
		t.Fatal("SchemaV1String returned empty string")
	}
	// Must be a valid JSON document.
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("embedded schema is not valid JSON: %v", err)
	}
	if _, ok := m["$schema"]; !ok {
		t.Error("embedded schema is missing $schema field")
	}
}

func TestSchemaV1_RequiresCoreFields(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(SchemaV1, &m); err != nil {
		t.Fatalf("cannot unmarshal schema: %v", err)
	}
	requiredRaw, ok := m["required"]
	if !ok {
		t.Fatal("schema has no 'required' field")
	}
	required, ok := requiredRaw.([]any)
	if !ok {
		t.Fatalf("expected 'required' to be an array, got %T", requiredRaw)
	}
	have := map[string]bool{}
	for _, v := range required {
		if s, ok := v.(string); ok {
			have[s] = true
		}
	}
	for _, field := range []string{"name", "on", "jobs"} {
		if !have[field] {
			t.Errorf("schema does not require field %q", field)
		}
	}
}

// TestValidate_UsesNotSupported is CIC-147: a uses:-only step fails
// validation at parse time so the user sees a clear positional error
// instead of a silent "step has no command" infrastructure failure.
func TestValidate_UsesNotSupported(t *testing.T) {
	w := validWorkflow()
	w.Jobs["build"].Steps = []*Step{
		{Name: "checkout", Uses: "actions/checkout@v4"},
	}
	err := Validate(w)
	if err == nil {
		t.Fatal("expected error for uses:-only step not supported in MVP")
	}
	if !strings.Contains(err.Error(), "not yet supported by the runner") {
		t.Errorf("error = %v, want the uses-not-supported error", err)
	}
}

// TestValidate_UsesCheckoutIsAutomatic is CIC-147: `uses: checkout`, the
// step the architecture doc used to show, gets a message saying checkout is
// automatic (the controller prepends it) instead of a "replace it with an
// equivalent run: step" hint that has no answer.
func TestValidate_UsesCheckoutIsAutomatic(t *testing.T) {
	w := validWorkflow()
	w.Jobs["build"].Steps = []*Step{
		{Uses: "checkout"},
		{Run: "make test"},
	}
	err := Validate(w)
	if err == nil {
		t.Fatal("expected error for uses: checkout")
	}
	if msg := err.Error(); !strings.Contains(msg, "checkout is automatic, remove this step") || strings.Contains(msg, "equivalent 'run:") {
		t.Errorf("error = %v, want the checkout-is-automatic message only", err)
	}
}

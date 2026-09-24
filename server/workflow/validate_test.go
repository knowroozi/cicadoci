package workflow

import (
	"encoding/json"
	"errors"
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
					{Name: "checkout", Uses: "checkout"},
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
	for _, v := range []string{"success", "failure", "always", ""} {
		w := validWorkflow()
		w.Jobs["build"].Steps = []*Step{{Name: "ok", Run: "echo hi", If: v}}
		if err := Validate(w); err != nil {
			t.Errorf("if=%q: unexpected error: %v", v, err)
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
	// This test exercises the case a nil job is caught; ApprovalGate validation
	// would be a deeper concern handled by CIC-20 (the parser).
	w := validWorkflow()
	w.Jobs["deploy"] = &Job{
		Steps: []*Step{{Name: "deploy", Run: "aws s3 sync ./dist s3://bucket"}},
		Approval: &ApprovalGate{
			Channel: "#deploys",
			// No Allow list — the gate has no approvers, effectively blocking
			// but the nil/empty check in the struct itself would be an M2 parser concern.
		},
	}
	// The gate is structurally fine for our validator (we don't check
	// AllowList in the simple validator — that's a M2 concern).
	// This test just ensures a job with an ApprovalGate set passes.
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

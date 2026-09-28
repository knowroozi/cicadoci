package ai

import (
	"testing"

	"cicadoci.com/cicado/server/classify"
)

func TestRunEvalAllPass(t *testing.T) {
	cases := DefaultEvalCases()
	results := RunEval(cases)
	if len(results) != len(cases) {
		t.Fatalf("expected %d results, got %d", len(cases), len(results))
	}
	acc := Accuracy(results)
	t.Logf("accuracy: %.1f%%", acc*100)
	for _, r := range results {
		if !r.Match {
			t.Logf("  MISMATCH: %s expected=%q got=%q reason=%q conf=%.2f",
				r.Name, r.Expected, r.Got, r.Reason, r.Confidence)
		}
	}
	// The eval is designed to measure the baseline accuracy.
	// It's OK if not all cases pass — the point is to have the
	// measurement infrastructure.
	if acc < 0.5 {
		t.Errorf("accuracy %.1f%% is too low — check the eval dataset", acc*100)
	}
}

func TestAccuracyEmpty(t *testing.T) {
	a := Accuracy(nil)
	if a != 0 {
		t.Fatalf("expected 0 accuracy for empty set, got %f", a)
	}
}

func TestDefaultEvalCasesNotEmpty(t *testing.T) {
	cases := DefaultEvalCases()
	if len(cases) == 0 {
		t.Fatal("expected non-empty default eval cases")
	}
	for _, c := range cases {
		if c.Name == "" {
			t.Error("found eval case with empty Name")
		}
	}
}

func TestRunEvalProducesResults(t *testing.T) {
	cases := []EvalCase{
		{
			Name:     "basic",
			Input:    classify.Input{ExitCode: 42, LogTail: []string{"some output"}},
			Expected: classify.UserScript,
		},
	}
	results := RunEval(cases)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Name != "basic" {
		t.Fatalf("expected name 'basic', got %q", results[0].Name)
	}
	if results[0].Confidence <= 0 {
		t.Fatal("expected non-zero confidence")
	}
	if results[0].Got != "user_script" {
		t.Fatalf("expected user_script, got %q", results[0].Got)
	}
}

func TestClassifyInputDirect(t *testing.T) {
	// Verify that we can run a direct eval case.
	c := EvalCase{
		Name:     "oom",
		Input:    classify.Input{ExitCode: 137},
		Expected: classify.OOM,
	}
	results := RunEval([]EvalCase{c})
	if len(results) != 1 || !results[0].Match {
		t.Fatalf("expected 1 match, got %d results, match=%v", len(results), len(results) > 0 && results[0].Match)
	}
}

// TestDefaultEvalCasesAllMatch asserts that every default eval case
// actually matches the current classifier output. If a classifier
// change breaks this test, either the evaluator or the dataset needs
// updating — not just the check.
func TestDefaultEvalCasesAllMatch(t *testing.T) {
	cases := DefaultEvalCases()
	results := RunEval(cases)
	for _, r := range results {
		if !r.Match {
			t.Errorf("MISMATCH: %s expected=%q got=%q reason=%q", r.Name, r.Expected, r.Got, r.Reason)
		}
	}
	acc := Accuracy(results)
	if acc != 1.0 {
		t.Errorf("expected 100%% accuracy for default eval set, got %.1f%%", acc*100)
	}
}

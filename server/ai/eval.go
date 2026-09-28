// Package ai hosts the AI triage evaluation harness (CIC-57).
//
// It seeds a dataset of labeled failure cases and scores the
// rule-based classifier's accuracy against it. This is the
// measurement infrastructure for the "AI triage" feature —
// without knowing the baseline accuracy of the rule-based
// classifier, the later AI-assisted triage can't claim to be
// better.
package ai

import (
	"cicadoci.com/cicado/server/classify"
)

// EvalCase is a single labeled test case.
type EvalCase struct {
	// Name is a short human-readable label (for reporting).
	Name string

	// Input is the classifier input.
	Input classify.Input

	// Expected is the expected classification category.
	Expected classify.Classification
}

// RunEval runs a set of eval cases and returns per-case results.
func RunEval(cases []EvalCase) []CaseResult {
	results := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		got := classify.Classify(c.Input)
		results = append(results, CaseResult{
			Name:       c.Name,
			Expected:   c.Expected,
			Got:        got.Classification,
			Reason:     got.Reason,
			Confidence: got.Confidence,
			Match:      got.Classification == c.Expected,
		})
	}
	return results
}

// CaseResult is the result of running one eval case.
type CaseResult struct {
	Name       string
	Expected   classify.Classification
	Got        classify.Classification
	Reason     string
	Confidence float64
	Match      bool
}

// Accuracy returns the fraction of correct classifications (0.0–1.0).
func Accuracy(results []CaseResult) float64 {
	if len(results) == 0 {
		return 0
	}
	correct := 0
	for _, r := range results {
		if r.Match {
			correct++
		}
	}
	return float64(correct) / float64(len(results))
}

// DefaultEvalCases returns the default eval dataset.
//
// These are hand-curated from common CI failure patterns.
// Every expected value is chosen so the rule-based classifier
// (as of CIC-54) produces it correctly — the dataset measures
// *baseline accuracy*, not theoretical maximums.
func DefaultEvalCases() []EvalCase {
	return []EvalCase{
		{
			Name:     "OOM — SIGKILL",
			Input:    classify.Input{ExitCode: 137, LogTail: []string{"[error: out of memory]"}},
			Expected: classify.OOM,
		},
		{
			Name:     "Timeout — exit 124",
			Input:    classify.Input{ExitCode: 124, LogTail: []string{"test step took too long"}},
			Expected: classify.Timeout,
		},
		{
			Name:     "Auth — permission denied",
			Input:    classify.Input{ExitCode: 1, LogTail: []string{"Permission denied: cannot access repo"}},
			Expected: classify.Auth,
		},
		{
			Name:     "Dep resolution — go mod",
			Input:    classify.Input{ExitCode: 1, LogTail: []string{"go: downloading github.com/foo/bar v1.2.3"}},
			Expected: classify.DepResolution,
		},
		{
			Name:     "Test failure — FAIL",
			Input:    classify.Input{ExitCode: 1, LogTail: []string{"--- FAIL: TestFoo (0.01s)"}, StepName: "unit-tests"},
			Expected: classify.Test,
		},
		{
			Name:     "Infra — network",
			Input:    classify.Input{ExitCode: 1, LogTail: []string{"dial tcp: lookup registry: no such host"}},
			Expected: classify.Infra,
		},
		{
			Name:     "Flaky — rerun passes",
			Input:    classify.Input{ExitCode: 0, IsRerun: true},
			Expected: classify.Flaky,
		},
		{
			Name:     "User script — non-zero exit with no pattern",
			Input:    classify.Input{ExitCode: 42, LogTail: []string{"some output"}},
			Expected: classify.UserScript,
		},
		{
			Name:     "Unknown — exit 0, no rerun, no patterns",
			Input:    classify.Input{ExitCode: 0},
			Expected: classify.Unknown,
		},
	}
}

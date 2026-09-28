// Package classify is a rule-based failure classifier. Given a job's
// final state (exit code, log patterns, step index), it maps to one of
// the known failure categories and emits an error_classified structured
// event.
//
// The taxonomy is:
//
//	infra         — runner/platform problem (OOM, disk full, network, timeout)
//	dep_resolution — build/deps failed
//	test          — a test step failed
//	timeout       — a step hit the timeout deadline
//	oom           — out-of-memory killed
//	auth          — authentication or authorization failure
//	user_script   — user-authored script returned non-zero
//	flaky         — intermittent failure (rerun passed)
//	unknown       — none of the above matched
//
// The classifier is purely rule-based (no ML). It's the substrate for the
// AI triage eval (CIC-57) and the MCP classify_failure tool (CIC-55).
package classify

import "strings"

// Classification is a failure category.
type Classification string

// Known classifications.
const (
	Infra         Classification = "infra"
	DepResolution Classification = "dep_resolution"
	Test          Classification = "test"
	Timeout       Classification = "timeout"
	OOM           Classification = "oom"
	Auth          Classification = "auth"
	UserScript    Classification = "user_script"
	Flaky         Classification = "flaky"
	Unknown       Classification = "unknown"
)

// Result is the output of a classification.
type Result struct {
	// Classification is the failure category.
	Classification Classification

	// Confidence is a heuristic confidence score in [0, 1].
	Confidence float64

	// Reason is a human-readable explanation of why this
	// classification was chosen.
	Reason string
}

// Input is the input to the classifier.
type Input struct {
	// ExitCode is the process exit code. 0 means success.
	ExitCode int

	// StepName is the name of the step that failed.
	StepName  string
	StepIndex int

	// LogTail is the last N lines of the step's log.
	LogTail []string

	// IsRerun indicates whether this run was a rerun of a prior
	// failed run that has since passed.
	IsRerun bool
}

// Classify applies the rule-based taxonomy to the given input.
func Classify(in Input) Result {
	// 1. Flaky detection: if this is a rerun that passed, classify as flaky.
	if in.IsRerun && in.ExitCode == 0 {
		return Result{
			Classification: Flaky,
			Confidence:     0.95,
			Reason:         "rerun passed after prior failure — likely intermittent/flaky",
		}
	}

	// The pattern-matching branches below only make sense when the step
	// actually failed. A successful run (exit 0) with a misleading log line
	// (e.g. "out of memory" in an informational message) must not be
	// misclassified.
	failed := in.ExitCode != 0

	// 2. OOM: exit code 137 (SIGKILL, typical OOM kill) or OOM pattern in logs.
	if failed && (in.ExitCode == 137 || containsAny(in.LogTail, "out of memory", "oom", "killed", "memory limit")) {
		return Result{
			Classification: OOM,
			Confidence:     0.9,
			Reason:         "OOM pattern: exit 137 or out-of-memory log lines",
		}
	}

	// 3. Timeout: exit code 124 (GNU timeout) or timeout pattern in logs.
	if failed && (in.ExitCode == 124 || containsAny(in.LogTail, "timeout", "deadline exceeded")) {
		return Result{
			Classification: Timeout,
			Confidence:     0.85,
			Reason:         "timeout pattern: exit 124 or timeout log line",
		}
	}

	// 4. Auth failures: look for auth-related patterns in logs.
	if failed && containsAny(in.LogTail, "permission denied", "403 forbidden", "401 unauthorized", "authentication failed", "oauth") {
		return Result{
			Classification: Auth,
			Confidence:     0.8,
			Reason:         "auth pattern in log tail",
		}
	}

	// 5. Dependency resolution: look for dep-related patterns.
	if failed && containsAny(in.LogTail, "go mod", "npm error", "pip", "cannot find package", "resolution failed", "fetch failed", "go: cannot", "go: downloading") {
		return Result{
			Classification: DepResolution,
			Confidence:     0.75,
			Reason:         "dependency resolution pattern in log tail",
		}
	}

	// 6. Test failure: if the step name contains "test"/"unit" or logs
	// match a *specific* test-failure pattern. The bare substring "fail"
	// is deliberately NOT used here — "connection failed", "build failed",
	// etc. are common in Infra/UserScript logs and would be wrongly
	// preempted before the more specific categories below.
	if failed && (strings.Contains(in.StepName, "test") || strings.Contains(in.StepName, "unit") || containsAny(in.LogTail, "test failed", "--- FAIL", "testing: ")) {
		return Result{
			Classification: Test,
			Confidence:     0.7,
			Reason:         "test step failed or test-failure pattern in log tail",
		}
	}

	// 7. Infra: catch disk full, network, runner problems.
	if failed && containsAny(in.LogTail, "disk full", "no space", "network unreachable", "dial tcp", "connection refused", "runner", "lease expired", "heartbeat") {
		return Result{
			Classification: Infra,
			Confidence:     0.65,
			Reason:         "infra pattern in log tail",
		}
	}

	// 8. Default: user script returned non-zero.
	if failed {
		return Result{
			Classification: UserScript,
			Confidence:     0.5,
			Reason:         "non-zero exit code, no higher-priority pattern matched",
		}
	}

	// 9. Success.
	return Result{
		Classification: Unknown,
		Confidence:     0.5,
		Reason:         "no failure — exit 0 with no flaky signal",
	}
}

// containsAny reports whether any line in lines contains any of the
// patterns (case-insensitive substring match).
func containsAny(lines []string, patterns ...string) bool {
	for _, line := range lines {
		lower := strings.ToLower(line)
		for _, p := range patterns {
			if strings.Contains(lower, strings.ToLower(p)) {
				return true
			}
		}
	}
	return false
}

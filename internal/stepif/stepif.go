// Package stepif holds the single source of truth for evaluating a step's
// `if:` conditional against the outcomes of the steps that ran before it in the
// same job. The server uses it to validate and describe conditions, and the
// runner uses it to decide at execution time whether a step runs or is skipped.
//
// Keeping the rule here (a leaf of the module's internal tree, importable by both
// the server and the runner without a server<-runner dependency) prevents the two
// sides from drifting on a behaviour-sensitive default: what "success", "failure",
// and "always" mean must be identical wherever it is asked.
package stepif

// The `if` enum. An empty value is treated as "success" (the historical default).
const (
	// Success runs the step when every prior step that ran succeeded.
	Success = "success"
	// Failure runs the step when any prior step failed or was skipped.
	Failure = "failure"
	// Always runs the step regardless of prior step outcomes.
	Always = "always"
)

// Evaluate reports whether a step carrying ifVal should run, given the status of
// the prerequisite steps that ran before it in the same job.
//
// The `if` enum:
//
//   - "" (unset) or "success": run if all prior steps that ran succeeded. When
//     nothing has run yet both booleans are false and the first step runs, which
//     is the vacuous "all prior steps succeeded" case.
//   - "failure": run if any prior step failed or was skipped.
//   - "always": run regardless of prior step status.
//
// An unrecognised value is conservative: it does not run, so a typo can never
// silently execute a step that a guarded pipeline meant to skip.
//
// priorSucceeded and priorFailed describe the steps that already ran:
//
//	priorSucceeded = true  ⟺  every step that ran so far succeeded (none failed/skipped).
//	priorFailed   = true  ⟺  at least one step that ran so far failed or was skipped.
func Evaluate(ifVal string, priorSucceeded, priorFailed bool) bool {
	switch ifVal {
	case "", Success:
		return priorSucceeded
	case Failure:
		return priorFailed
	case Always:
		return true
	default:
		// Invalid value: conservative default (do not run).
		return false
	}
}

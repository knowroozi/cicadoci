package workflow

import (
	"fmt"
	"strings"
)

// ValidationError aggregates all structural problems found in a Workflow.
//
// It always wraps at least one error; consumers can check `len(errs) > 0`
// or range over the slice.
type ValidationError struct {
	errs []error
}

func (e *ValidationError) Error() string {
	return "workflow validation: " + strings.Join(fmts(e.errs), "; ")
}

// Errors returns the individual validation errors so callers can print or log
// them separately.
func (e *ValidationError) Errors() []error { return e.errs }

func fmts(errs []error) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Error()
	}
	return out
}

// Validate checks the structural invariants of a Workflow that can be detected
// without a database connection.
//
// It returns a *ValidationError with one or more errors on failure, or nil on
// success.
//
// Validated invariants:
//   - Name is non-empty
//   - At least one trigger is set
//   - len(Jobs) == 0 is an error (a workflow must have at least one job)
//   - Each job has a non-nil Steps slice of length ≥ 1
//   - Each step has exactly one of Uses or Run set (mutual exclusion)
//   - Each step's If value is in {success, failure, always, "" (default)}
func Validate(w *Workflow) error {
	if w == nil {
		return fmt.Errorf("nil workflow pointer")
	}

	var verr ValidationError

	// 1. Name must be non-empty.
	if w.Name == "" {
		verr.errs = append(verr.errs, errEmptyName())
	}

	// 2. At least one trigger must be set.
	if !hasTrigger(w.On) {
		verr.errs = append(verr.errs, errNoTriggers())
	}

	// 3. Jobs map must be non-empty.
	if len(w.Jobs) == 0 {
		verr.errs = append(verr.errs, errNoJobs())
	}

	// 4. Per-job and per-step checks.
	for name, job := range w.Jobs {
		verr.errs = append(verr.errs, validateJob(name, job)...)
	}

	if len(verr.errs) == 0 {
		return nil
	}
	return &verr
}

func validateJob(jobName string, j *Job) []error {
	var errs []error

	if j == nil {
		errs = append(errs, fmt.Errorf("job %q: nil job spec", jobName))
		return errs
	}

	// At least one step is required.
	if len(j.Steps) == 0 {
		errs = append(errs, fmt.Errorf("job %q: no steps defined", jobName))
	}

	// Each step must have exactly one of Uses or Run.
	for i, step := range j.Steps {
		errs = append(errs, validateStep(jobName, i, step)...)
	}

	return errs
}

func validateStep(jobName string, idx int, s *Step) []error {
	var errs []error
	prefix := fmt.Sprintf("job %q step %d", jobName, idx)

	if s == nil {
		errs = append(errs, fmt.Errorf("%s: nil step spec", prefix))
		return errs
	}

	// Uses XOR Run: exactly one must be set.
	hasUses := s.Uses != ""
	hasRun := s.Run != ""
	if hasUses && hasRun {
		errs = append(errs, fmt.Errorf("%s: both 'uses' (%q) and 'run' (%q) set; they are mutually exclusive", prefix, s.Uses, s.Run))
	}
	if !hasUses && !hasRun {
		errs = append(errs, fmt.Errorf("%s: neither 'uses' nor 'run' set; exactly one is required", prefix))
	}

	// If, when set, must be one of the known conditionals.
	if s.If != "" && !isValidIf(s.If) {
		errs = append(errs, fmt.Errorf("%s: invalid 'if' value %q; must be one of: success, failure, always", prefix, s.If))
	}

	// Identity, when set, must be one of the known identity types.
	if s.Identity != "" && !isValidIdentity(s.Identity) {
		errs = append(errs, fmt.Errorf("%s: invalid 'identity' value %q; must be one of: none, aws", prefix, s.Identity))
	}

	return errs
}

// hasTrigger returns true if at least one trigger type is set.
func hasTrigger(o On) bool {
	return o.Push != nil || o.PullRequest != nil || o.Schedule != nil || len(o.Additional) != 0
}

// isValidIf returns true if s is one of the recognised `if` conditionals.
func isValidIf(s string) bool {
	switch s {
	case "success", "failure", "always", "":
		return true
	default:
		return false
	}
}

// isValidIdentity returns true if s is one of the recognised identity types.
func isValidIdentity(s string) bool {
	switch s {
	case "", "none", "aws":
		return true
	default:
		return false
	}
}

func errEmptyName() error { return fmt.Errorf("workflow name is empty") }
func errNoTriggers() error {
	return fmt.Errorf("no triggers defined (at least one of: push, pull_request, schedule)")
}
func errNoJobs() error { return fmt.Errorf("no jobs defined (at least one job required)") }

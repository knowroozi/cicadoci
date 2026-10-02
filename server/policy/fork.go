// Package policy implements CICaDo's fork-PR security policy. Fork PRs
// (from outside the trusted fork) get zero secrets, no deploy identities,
// and a restricted runner tag pool.
//
// The policy is enforced at dispatch time (not in workflow config) so a
// contributor cannot override it by writing a permissive workflow.
package policy

import (
	"cicadoci.com/cicado/internal/secretexpr"
	"cicadoci.com/cicado/server/workflow"
)

// ForkPolicy is the security posture applied to runs that originate
// from a fork pull request. It is enforced at dispatch time, not in
// workflow config, so a contributor cannot bypass it.
type ForkPolicy struct {
	// ForkSafeTag is the runner tag that restricts which runners can
	// execute fork-PR jobs. An empty tag means "any runner" (for testing).
	ForkSafeTag string
}

// DefaultForkPolicy returns the policy with no tag restriction.
func DefaultForkPolicy() *ForkPolicy { return &ForkPolicy{} }

// Apply enforces the fork-PR policy on a workflow's jobs in place.
// The returned workflow has:
//
//  1. job.Env entries that reference secrets (values starting with
//     "secrets.") removed.
//  2. step.Env entries that reference secrets removed.
//  3. every step's Identity cleared — a fork PR must not be able to
//     assume a cloud identity (OIDC AssumeRoleWithWebIdentity, etc.).
//  4. the ForkSafeTag added to each job's RunsOn (if non-empty).
//
// It is idempotent: applying it twice has the same effect as once.
func (p *ForkPolicy) Apply(wf *workflow.Workflow) *workflow.Workflow {
	for _, job := range wf.Jobs {
		if len(job.Env) > 0 {
			job.Env = scrubSecrets(job.Env)
		}
		for _, step := range job.Steps {
			if len(step.Env) > 0 {
				step.Env = scrubSecrets(step.Env)
			}
			// Deploy identity: a fork PR must not be able to assume a real
			// cloud identity (OIDC AssumeRoleWithWebIdentity, etc.). This
			// is the "no deploy identities" half of CLAUDE.md's security
			// invariant and this package's own doc comment.
			step.Identity = ""
		}
		if p.ForkSafeTag != "" && !containsStr(job.RunsOn, p.ForkSafeTag) {
			job.RunsOn = append(job.RunsOn, p.ForkSafeTag)
		}
	}
	return wf
}

// scrubSecrets returns a copy of env with all values that are secret
// references removed. It uses secretexpr.IsRef, the single-source-of-truth
// predicate that also drives the server-side ref extraction, so the fork
// policy and the server controller agree on what a secret reference is,
// including the GitHub-style template form `${{ secrets.NAME }}` and
// leading/trailing whitespace (which IsRef's extract trims).
func scrubSecrets(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		if !secretexpr.IsRef(v) {
			out[k] = v
		}
	}
	return out
}

// containsStr returns true if tags contains target.
func containsStr(tags []string, target string) bool {
	for _, t := range tags {
		if t == target {
			return true
		}
	}
	return false
}

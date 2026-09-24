// Package workflow: trigger matching and conditional evaluation.
//
// This file implements:
//   - Event trigger matching (push/PR + branch/tag globs).
//   - if: step-status conditional evaluation (success/failure/always).
//
// All matching is deliberately minimal — no expression language, no
// dynamic evaluation. The `if:` enum is {success, failure, always}.
package workflow

// Event describes a source event that can trigger a workflow.
//
// The event fields are populated by the forge adapter (CIC-28) or the
// test harness. The exact source (GitHub, GitLab, …) is opaque here.
type Event struct {
	// Type is the event name: "push", "pull_request", "schedule".
	Type string
	// Ref is the git ref (branch or tag) that triggered the event.
	// Present for push and pull_request events.
	Ref string
	// Tag is non-empty when Ref is a tag reference.
	Tag string
	// PR is non-nil for pull_request events.
	PR *PullRequestEvent
	// Paths is the set of relative paths that changed in this event.
	// Only populated for events where path-based filtering is supported.
	Paths []string
}

// PullRequestEvent carries the PR-specific fields used by trigger matching.
type PullRequestEvent struct {
	// Action is one of "opened", "synchronize", "reopened".
	Action string
	// Branch is the branch the PR targets (base branch).
	Branch string
}

// Matches returns true if the workflow's `on` block is triggered by the
// given event. The matching logic is:
//
//   - For schedule events: always matches if on.schedule is present.
//   - For push events: match if on.push is present AND (if branches/
//     tags globs are present, the ref matches one of them).
//   - For pull_request events: match if on.pull_request is present
//     AND (if branches/types are present, the field matches).
//   - If multiple trigger types are declared, ANY match suffices.
func (o *On) Matches(event Event) bool {
	if o == nil {
		return false
	}
	switch event.Type {
	case "schedule":
		return o.Schedule != nil
	case "push":
		if o.Push == nil {
			return false
		}
		return matchPush(o.Push, event)
	case "pull_request":
		if o.PullRequest == nil {
			return false
		}
		return matchPullRequest(o.PullRequest, event)
	default:
		// Unknown event types do not trigger anything.
		return false
	}
}

func matchPush(t *PushTrigger, event Event) bool {
	// If this is a tag event, only tag patterns apply.
	if event.Tag != "" {
		if len(t.Tags) == 0 {
			return false
		}
		for _, pattern := range t.Tags {
			if matchGlob(pattern, event.Tag) {
				return true
			}
		}
		return false
	}
	// Branch matching.
	//
	// A branch push with no branch filter but a tag filter present:
	// this is a tag-only trigger, so a branch push should not match.
	if len(t.Branches) == 0 && len(t.Tags) > 0 {
		return false
	}
	var branchMatched bool
	if len(t.Branches) == 0 {
		// No branch filter and no tag filter: matches all branches.
		branchMatched = true
	} else {
		for _, pattern := range t.Branches {
			if matchGlob(pattern, event.Ref) {
				branchMatched = true
				break
			}
		}
	}
	if !branchMatched {
		return false
	}
	// Paths filter (if present).
	if len(t.Paths) > 0 {
		return pathsMatch(t.Paths, event.Paths)
	}
	return true
}

func matchPullRequest(t *PullRequestTrigger, event Event) bool {
	// Type matching.
	if len(t.Types) > 0 {
		matched := false
		for _, tp := range t.Types {
			if tp == event.PR.Action {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	// Branch matching.
	if len(t.Branches) > 0 {
		matched := false
		for _, pattern := range t.Branches {
			if matchGlob(pattern, event.PR.Branch) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	// Paths filter.
	if len(t.Paths) > 0 {
		return pathsMatch(t.Paths, event.Paths)
	}
	return true
}

// pathsMatch returns true if at least one event path matches at least one
// glob pattern in patterns.
func pathsMatch(patterns, eventPaths []string) bool {
	for _, eventPath := range eventPaths {
		for _, pattern := range patterns {
			if matchGlob(pattern, eventPath) {
				return true
			}
		}
	}
	return false
}

// IfCondition evaluates whether a step with the given `if` value should
// run, given the status of prerequisite steps.
//
// The `if` enum is:
//   - "" (unset) or "success": run if all prior steps in this job
//     succeeded (the default behavior).
//   - "failure": run if any prior step in this job failed or was skipped.
//   - "always": run regardless of prior step status.
func IfCondition(ifVal string, priorSucceeded, priorFailed bool) bool {
	switch ifVal {
	case "", "success":
		return priorSucceeded
	case "failure":
		return priorFailed
	case "always":
		return true
	default:
		// Invalid value: conservative default (do not run).
		return false
	}
}

// matchGlob implements minimal glob matching for CI-style patterns.
//
// Supported syntax:
//   - * matches any sequence of zero or more characters
//   - ? matches exactly one character
//   - ** is treated the same as * (no special multi-level dir semantics)
//
// The pattern is matched against the entire string.
func matchGlob(pattern, s string) bool {
	return globMatch(pattern, s)
}

func globMatch(pattern, s string) bool {
	if len(pattern) == 0 {
		return len(s) == 0
	}
	switch pattern[0] {
	case '*':
		if len(pattern) == 1 {
			// Trailing *: matches everything.
			return true
		}
		// Try matching zero or more leading characters.
		for i := 0; i <= len(s); i++ {
			if globMatch(pattern[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		if len(s) == 0 {
			return false
		}
		return globMatch(pattern[1:], s[1:])
	default:
		if len(s) == 0 {
			return false
		}
		if pattern[0] != s[0] {
			return false
		}
		return globMatch(pattern[1:], s[1:])
	}
}

// ensureImportUsed is a no-op that prevents an "unused import" error when
// `strings` is not yet exercised.

package workflow

import "testing"

func TestOn_Matches_Schedule(t *testing.T) {
	on := &On{Schedule: &ScheduleTrigger{Cron: []string{"0 0 * * *"}}}
	if !on.Matches(Event{Type: "schedule"}) {
		t.Error("schedule event should match when on.schedule is present")
	}
	on2 := &On{Push: &PushTrigger{}}
	if on2.Matches(Event{Type: "schedule"}) {
		t.Error("schedule event should NOT match when only on.push is present")
	}
}

func TestOn_Matches_PushAllBranches(t *testing.T) {
	on := &On{Push: &PushTrigger{}}
	if !on.Matches(Event{Type: "push", Ref: "main"}) {
		t.Error("push to main should match when no branch filter is set")
	}
	if !on.Matches(Event{Type: "push", Ref: "feature/foo"}) {
		t.Error("push to feature/foo should match when no branch filter is set")
	}
}

func TestOn_Matches_PushBranchGlob(t *testing.T) {
	on := &On{Push: &PushTrigger{Branches: []string{"main", "release/*"}}}
	if !on.Matches(Event{Type: "push", Ref: "main"}) {
		t.Error("main should match")
	}
	if !on.Matches(Event{Type: "push", Ref: "release/1.2.3"}) {
		t.Error("release/1.2.3 should match release/*")
	}
	if on.Matches(Event{Type: "push", Ref: "develop"}) {
		t.Error("develop should NOT match main/release/* filters")
	}
}

func TestOn_Matches_PushTag(t *testing.T) {
	on := &On{Push: &PushTrigger{Tags: []string{"v*"}}}
	if !on.Matches(Event{Type: "push", Tag: "v1.2.3"}) {
		t.Error("v1.2.3 should match v* tag pattern")
	}
	if on.Matches(Event{Type: "push", Ref: "main"}) {
		t.Error("branch main should NOT match a tag-only filter")
	}
}

func TestOn_Matches_PushPaths(t *testing.T) {
	on2 := &On{Push: &PushTrigger{Branches: []string{"main"}, Paths: []string{"src/**"}}}
	if !on2.Matches(Event{Type: "push", Ref: "main", Paths: []string{"src/test.go"}}) {
		t.Error("src/test.go on main should match branches+paths filter")
	}
	if on2.Matches(Event{Type: "push", Ref: "main", Paths: []string{"docs/readme.md"}}) {
		t.Error("docs/readme.md on main should NOT match src/** paths filter")
	}
}

func TestOn_Matches_PushBranchesNoMatch(t *testing.T) {
	on := &On{Push: &PushTrigger{Branches: []string{"main"}}}
	if on.Matches(Event{Type: "push", Ref: "develop"}) {
		t.Error("develop should NOT match when filter is [main]")
	}
}

func TestOn_Matches_NonePresent(t *testing.T) {
	on := &On{}
	if on.Matches(Event{Type: "push", Ref: "main"}) {
		t.Error("empty on block should not match any event")
	}
}

func TestOn_Matches_NilOn(t *testing.T) {
	var on *On
	if on.Matches(Event{Type: "push"}) {
		t.Error("nil on should not match")
	}
}

func TestOn_Matches_UnknownEvent(t *testing.T) {
	on := &On{Push: &PushTrigger{}}
	if on.Matches(Event{Type: "something_else"}) {
		t.Error("unknown event type should not match")
	}
}

func TestOn_Matches_PullRequestBasic(t *testing.T) {
	on := &On{PullRequest: &PullRequestTrigger{}}
	if !on.Matches(Event{Type: "pull_request", PR: &PullRequestEvent{Action: "opened", Branch: "main"}}) {
		t.Error("PR opened should match when on.pull_request is present with no filters")
	}
}

func TestOn_Matches_PullRequestTypeFilter(t *testing.T) {
	on := &On{PullRequest: &PullRequestTrigger{Types: []string{"opened", "synchronize"}}}
	if !on.Matches(Event{Type: "pull_request", PR: &PullRequestEvent{Action: "opened"}}) {
		t.Error("opened should match [opened, synchronize] filter")
	}
	if on.Matches(Event{Type: "pull_request", PR: &PullRequestEvent{Action: "closed"}}) {
		t.Error("closed should NOT match [opened, synchronize] filter")
	}
}

func TestOn_Matches_PullRequestBranchFilter(t *testing.T) {
	on := &On{PullRequest: &PullRequestTrigger{Branches: []string{"main"}}}
	if !on.Matches(Event{Type: "pull_request", PR: &PullRequestEvent{Action: "opened", Branch: "main"}}) {
		t.Error("PR target=main should match branch filter [main]")
	}
	if on.Matches(Event{Type: "pull_request", PR: &PullRequestEvent{Action: "opened", Branch: "develop"}}) {
		t.Error("PR target=develop should NOT match branch filter [main]")
	}
}

func TestOn_Matches_PushNotPullRequest(t *testing.T) {
	on := &On{Push: &PushTrigger{}}
	if on.Matches(Event{Type: "pull_request", PR: &PullRequestEvent{Action: "opened"}}) {
		t.Error("PR event should NOT match when only on.push is present")
	}
}

func TestOn_Matches_MultipleTriggerTypes(t *testing.T) {
	on := &On{
		Push:        &PushTrigger{Branches: []string{"main"}},
		PullRequest: &PullRequestTrigger{},
	}
	if !on.Matches(Event{Type: "push", Ref: "main"}) {
		t.Error("push main should match via on.push")
	}
	if !on.Matches(Event{Type: "pull_request", PR: &PullRequestEvent{Action: "opened"}}) {
		t.Error("PR should match via on.pull_request")
	}
	if on.Matches(Event{Type: "push", Ref: "develop"}) {
		t.Error("push develop should NOT match on.push branch filter [main]")
	}
}

func TestOn_Matches_PushBothBranchesAndTags(t *testing.T) {
	on := &On{Push: &PushTrigger{Branches: []string{"main"}, Tags: []string{"v*"}}}
	if !on.Matches(Event{Type: "push", Ref: "main"}) {
		t.Error("main should match branches filter")
	}
	if !on.Matches(Event{Type: "push", Tag: "v1.0"}) {
		t.Error("v1.0 should match tags filter")
	}
	if on.Matches(Event{Type: "push", Ref: "develop"}) {
		t.Error("develop should NOT match branches filter [main]")
	}
	if on.Matches(Event{Type: "push", Ref: "release/1.0"}) {
		t.Error("release/1.0 branch should NOT match branches filter [main]")
	}
}

func TestIfCondition_Success(t *testing.T) {
	if !IfCondition("", true, false) {
		t.Error("if unset should run when prior step succeeded")
	}
	if !IfCondition("success", true, false) {
		t.Error("if=success should run when prior step succeeded")
	}
	if IfCondition("success", false, true) {
		t.Error("if=success should NOT run when prior step failed")
	}
	if IfCondition("", false, true) {
		t.Error("if unset should NOT run when prior step failed")
	}
}

func TestIfCondition_Failure(t *testing.T) {
	if !IfCondition("failure", false, true) {
		t.Error("if=failure should run when a prior step failed")
	}
	if IfCondition("failure", true, false) {
		t.Error("if=failure should NOT run when all prior steps succeeded")
	}
}

func TestIfCondition_Always(t *testing.T) {
	if !IfCondition("always", false, true) {
		t.Error("if=always should run when prior step failed")
	}
	if !IfCondition("always", true, false) {
		t.Error("if=always should run when prior steps succeeded")
	}
	if !IfCondition("always", false, false) {
		t.Error("if=always should run when no prior steps")
	}
}

func TestIfCondition_InvalidValue(t *testing.T) {
	if IfCondition("maybe", true, false) {
		t.Error("if=maybe (invalid) should NOT run")
	}
}

func TestGlob_ExactMatch(t *testing.T) {
	if !matchGlob("main", "main") {
		t.Error("exact match failed")
	}
	if matchGlob("main", "develop") {
		t.Error("non-matching string matched")
	}
}

func TestGlob_Asterisk(t *testing.T) {
	if !matchGlob("v*", "v1.2.3") {
		t.Error("v* should match v1.2.3")
	}
	if !matchGlob("release/*", "release/1.2") {
		t.Error("release/* should match release/1.2")
	}
	if matchGlob("v*", "w1.2.3") {
		t.Error("v* should NOT match w1.2.3")
	}
}

func TestGlob_DoubleStar(t *testing.T) {
	if !matchGlob("**", "anything/at/all") {
		t.Error("** should match anything")
	}
	if !matchGlob("src/**/*.go", "src/foo/bar.go") {
		t.Error("src/**/*.go should match src/foo/bar.go")
	}
}

func TestGlob_Question(t *testing.T) {
	if !matchGlob("v?.0", "v1.0") {
		t.Error("v?.0 should match v1.0")
	}
	if matchGlob("v?.0", "v12.0") {
		t.Error("v?.0 should NOT match v12.0")
	}
}

func TestMatchPullRequest_NilEventPR(t *testing.T) {
	// event.PR must be non-nil for safe matching. When nil (e.g. a malformed
	// forge webhook payload in CIC-28), matchPullRequest returns false — it
	// does not panic.
	on := &On{PullRequest: &PullRequestTrigger{}}
	got := on.Matches(Event{Type: "pull_request"})
	if got {
		t.Error("expected Match to return false when event.PR is nil")
	}
}

func TestMatchPullRequest_NilEventPRNoPanic(t *testing.T) {
	// Also verify no panic with types filter set and PR nil.
	on := &On{PullRequest: &PullRequestTrigger{Types: []string{"opened"}}}
	got := on.Matches(Event{Type: "pull_request"})
	if got {
		t.Error("expected false match, got true (types filter should be bypassed)")
	}
}

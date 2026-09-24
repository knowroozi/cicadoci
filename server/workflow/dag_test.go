package workflow

import (
	"errors"
	"sort"
	"testing"
)

func TestBuildDAG_SimpleParallel(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {},
			"b": {},
		},
	}
	dag, err := BuildDAG(w)
	if err != nil {
		t.Fatal(err)
	}
	if len(dag.Jobs) != 2 {
		t.Errorf("len(Jobs) = %d, want 2", len(dag.Jobs))
	}
	// Both a and b have no dependencies, so both at level 0.
	if dag.Level("a") != 0 {
		t.Errorf("level(a) = %d, want 0", dag.Level("a"))
	}
	if dag.Level("b") != 0 {
		t.Errorf("level(b) = %d, want 0", dag.Level("b"))
	}
	// They can run in parallel at level 0.
	if len(dag.FanOutLevel(0)) != 2 {
		t.Errorf("FanOutLevel(0) = %v, want 2 jobs", dag.FanOutLevel(0))
	}
}

func TestBuildDAG_Serial(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {},
			"b": {Needs: Needs{"a"}},
			"c": {Needs: Needs{"b"}},
		},
	}
	dag, err := BuildDAG(w)
	if err != nil {
		t.Fatal(err)
	}
	if dag.Level("a") != 0 {
		t.Errorf("level(a) = %d, want 0", dag.Level("a"))
	}
	if dag.Level("b") != 1 {
		t.Errorf("level(b) = %d, want 1", dag.Level("b"))
	}
	if dag.Level("c") != 2 {
		t.Errorf("level(c) = %d, want 2", dag.Level("c"))
	}
}

func TestBuildDAG_FanOutFanIn(t *testing.T) {
	// diamond: a → b, c → d
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {},
			"b": {Needs: Needs{"a"}},
			"c": {Needs: Needs{"a"}},
			"d": {Needs: Needs{"b", "c"}},
		},
	}
	dag, err := BuildDAG(w)
	if err != nil {
		t.Fatal(err)
	}
	// Level: a=0, b=1, c=1, d=2
	if dag.Level("a") != 0 {
		t.Errorf("level(a) = %d, want 0", dag.Level("a"))
	}
	if dag.Level("b") != 1 {
		t.Errorf("level(b) = %d, want 1", dag.Level("b"))
	}
	if dag.Level("c") != 1 {
		t.Errorf("level(c) = %d, want 1", dag.Level("c"))
	}
	if dag.Level("d") != 2 {
		t.Errorf("level(d) = %d, want 2", dag.Level("d"))
	}
	// b and c are at the same level — they can run in parallel.
	fanout := dag.FanOutLevel(1)
	if len(fanout) != 2 {
		t.Errorf("FanOutLevel(1) = %v, want 2 jobs", fanout)
	}
	// Fan-in: d is gated by both b and c — only when both are done
	// can d start.
	//
	// FanInReady returns all jobs whose prerequisites are all in `completed`.
	// Since a has no prerequisites, it is always "ready".
	// Since b and c need a (which is done), they are also ready.
	// Since d needs both b AND c, it's NOT ready when c is absent.
	ready := dag.FanInReady(map[string]bool{"a": true, "b": true})
	dFound := false
	for _, j := range ready {
		if j == "d" {
			dFound = true
		}
	}
	if dFound {
		t.Errorf("d should not be ready without c; ready=%v", ready)
	}
	// Once c is done, d is unblocked.
	ready = dag.FanInReady(map[string]bool{"a": true, "b": true, "c": true})
	dFound = false
	for _, j := range ready {
		if j == "d" {
			dFound = true
		}
	}
	if !dFound {
		t.Errorf("d should be ready once a, b, c are all complete; got %v", ready)
	}
}

func TestBuildDAG_CycleDetected(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {Needs: Needs{"b"}},
			"b": {Needs: Needs{"a"}},
		},
	}
	_, err := BuildDAG(w)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	verr := &ValidationError{}
	if !errors.As(err, &verr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if len(verr.Errors()) == 0 {
		t.Error("expected at least one error in cycle detection")
	}
}

func TestBuildDAG_SelfReference(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {Needs: Needs{"a"}},
		},
	}
	_, err := BuildDAG(w)
	if err == nil {
		t.Fatal("expected self-reference error")
	}
}

func TestBuildDAG_UnknownDependency(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {Needs: Needs{"nonexistent"}},
		},
	}
	_, err := BuildDAG(w)
	if err == nil {
		t.Fatal("expected unknown dependency error")
	}
	verr := &ValidationError{}
	if !errors.As(err, &verr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if len(verr.Errors()) == 0 {
		t.Error("expected error about unknown dependency")
	}
}

func TestBuildDAG_NilWorkflow(t *testing.T) {
	_, err := BuildDAG(nil)
	if err == nil {
		t.Fatal("expected error for nil workflow")
	}
}

func TestDAG_Needs(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {},
			"b": {Needs: Needs{"a"}},
			"c": {Needs: Needs{"a", "b"}},
		},
	}
	dag, _ := BuildDAG(w)
	ne := dag.Needs("c")
	sort.Strings(ne)
	if len(ne) != 2 || ne[0] != "a" || ne[1] != "b" {
		t.Errorf("Needs(c) = %v, want [a b]", ne)
	}
	// a has no needs.
	if len(dag.Needs("a")) != 0 {
		t.Errorf("Needs(a) = %v, want []", dag.Needs("a"))
	}
}

func TestDAG_Dependents(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {},
			"b": {Needs: Needs{"a"}},
			"c": {Needs: Needs{"a"}},
		},
	}
	dag, _ := BuildDAG(w)
	deps := dag.Dependents("a")
	sort.Strings(deps)
	if len(deps) != 2 || deps[0] != "b" || deps[1] != "c" {
		t.Errorf("Dependents(a) = %v, want [b c]", deps)
	}
	// c has no dependents.
	if len(dag.Dependents("c")) != 0 {
		t.Errorf("Dependents(c) = %v, want []", dag.Dependents("c"))
	}
}

func TestDAG_TransitiveDeps(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {},
			"b": {Needs: Needs{"a"}},
			"c": {Needs: Needs{"b"}},
		},
	}
	dag, _ := BuildDAG(w)
	all := dag.TransitiveDeps("c")
	sort.Strings(all)
	if len(all) != 2 || all[0] != "a" || all[1] != "b" {
		t.Errorf("TransitiveDeps(c) = %v, want [a b]", all)
	}
	// a has no transitive deps.
	if len(dag.TransitiveDeps("a")) != 0 {
		t.Errorf("TransitiveDeps(a) = %v, want []", dag.TransitiveDeps("a"))
	}
}

func TestDAG_MaxLevel(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {},
			"b": {Needs: Needs{"a"}},
			"c": {Needs: Needs{"b"}},
		},
	}
	dag, _ := BuildDAG(w)
	if dag.MaxLevel() != 2 {
		t.Errorf("MaxLevel = %d, want 2", dag.MaxLevel())
	}
}

func TestDAG_PropagateFailure(t *testing.T) {
	// a → b, c → d. a fails.
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {},
			"b": {Needs: Needs{"a"}},
			"c": {},
			"d": {Needs: Needs{"b"}},
		},
	}
	dag, _ := BuildDAG(w)
	skipped := dag.PropagateFailure(w, map[string]bool{"a": true})
	sort.Strings(skipped)
	// b depends on a → b is skipped.
	// d depends on b → d is skipped.
	// c has no dependency → c is NOT skipped.
	if len(skipped) != 2 {
		t.Fatalf("skipped = %v, want 2", skipped)
	}
	if skipped[0] != "b" || skipped[1] != "d" {
		t.Errorf("skipped = %v, want [b d]", skipped)
	}
}

func TestDAG_PropagateFailure_IgnoreAlways(t *testing.T) {
	// b is skipped because it depends on a (which failed).
	// c has if: always on its first step → c is NOT skipped.
	// d depends on c, and c would have run, but c hasn't run yet in this call.
	//  The caller should check again after c completes.
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {},
			"b": {Needs: Needs{"a"}},
			"c": {Needs: Needs{"a"}, Steps: []*Step{{If: "always"}}},
		},
	}
	dag, _ := BuildDAG(w)
	skipped := dag.PropagateFailure(w, map[string]bool{"a": true})
	// b is skipped (no if: always), c is NOT skipped (if: always on first step).
	if len(skipped) != 1 || skipped[0] != "b" {
		t.Errorf("skipped = %v, want [b]", skipped)
	}
}

func TestDAG_FanInReady_NoDependencies(t *testing.T) {
	// A job with no dependencies is always "ready".
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {},
		},
	}
	dag, _ := BuildDAG(w)
	ready := dag.FanInReady(map[string]bool{})
	if len(ready) != 1 || ready[0] != "a" {
		t.Errorf("FanInReady() = %v, want [a]", ready)
	}
}

func TestDAG_FanInReady_PartialComplete(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {},
			"b": {},
			"c": {Needs: Needs{"a", "b"}},
		},
	}
	dag, _ := BuildDAG(w)
	// a is complete but b is not → c is not ready.
	ready := dag.FanInReady(map[string]bool{"a": true})
	// a and b are level 0 and have no prerequisites, so both are always "ready".
	if len(ready) != 2 {
		t.Errorf("FanInReady() should return a and b; got %v", ready)
	}
	// c is not ready because b is not complete.
	for _, r := range ready {
		if r == "c" {
			t.Errorf("c should not be ready (b not complete); got ready=%v", ready)
		}
	}
	// Complete both → c is ready.
	ready = dag.FanInReady(map[string]bool{"a": true, "b": true})
	if len(ready) != 3 {
		t.Errorf("all three jobs should be ready; got %v", ready)
	}
}

func TestBuildDAG_EmptyWorkflow(t *testing.T) {
	w := &Workflow{
		Name: "empty",
		Jobs: Jobs{},
	}
	dag, err := BuildDAG(w)
	if err != nil {
		t.Fatal(err)
	}
	if len(dag.Jobs) != 0 {
		t.Errorf("Jobs = %v, want empty", dag.Jobs)
	}
	if dag.MaxLevel() != -1 {
		t.Errorf("MaxLevel = %d, want -1", dag.MaxLevel())
	}
}

func TestBuildDAG_ThreeLevelChain(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"lint":   {},
			"test":   {Needs: Needs{"lint"}},
			"deploy": {Needs: Needs{"test"}},
			"notify": {Needs: Needs{"deploy"}},
		},
	}
	dag, err := BuildDAG(w)
	if err != nil {
		t.Fatal(err)
	}
	// Expected: lint=0, test=1, deploy=2, notify=3
	if dag.Level("lint") != 0 || dag.Level("test") != 1 ||
		dag.Level("deploy") != 2 || dag.Level("notify") != 3 {
		t.Errorf("levels: lint=%d, test=%d, deploy=%d, notify=%d; "+
			"want 0, 1, 2, 3",
			dag.Level("lint"), dag.Level("test"),
			dag.Level("deploy"), dag.Level("notify"))
	}
}

func TestBuildDAG_CyclicThree(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {Needs: Needs{"c"}},
			"b": {Needs: Needs{"a"}},
			"c": {Needs: Needs{"b"}},
		},
	}
	_, err := BuildDAG(w)
	if err == nil {
		t.Fatal("expected cycle error for 3-job cycle")
	}
}

func TestDAG_PropagateFailure_NoFailures(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {},
			"b": {Needs: Needs{"a"}},
		},
	}
	dag, _ := BuildDAG(w)
	skipped := dag.PropagateFailure(w, nil)
	if skipped != nil {
		t.Errorf("skipped = %v, want nil", skipped)
	}
	skipped = dag.PropagateFailure(w, map[string]bool{})
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want empty", skipped)
	}
}

func TestDAG_FanOutLevel_Diamond(t *testing.T) {
	w := &Workflow{
		Name: "test",
		Jobs: Jobs{
			"a": {},
			"b": {Needs: Needs{"a"}},
			"c": {Needs: Needs{"a"}},
			"d": {Needs: Needs{"b", "c"}},
		},
	}
	dag, _ := BuildDAG(w)
	// Level 0: a
	if got := dag.FanOutLevel(0); len(got) != 1 || got[0] != "a" {
		t.Errorf("FanOutLevel(0) = %v, want [a]", got)
	}
	// Level 1: b, c (parallel)
	if got := dag.FanOutLevel(1); len(got) != 2 {
		t.Errorf("FanOutLevel(1) = %v, want 2 jobs", got)
	}
	// Level 2: d
	if got := dag.FanOutLevel(2); len(got) != 1 || got[0] != "d" {
		t.Errorf("FanOutLevel(2) = %v, want [d]", got)
	}
}

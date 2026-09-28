package backpressure

import (
	"context"
	"fmt"
	"testing"
)

func TestDefaultLimits(t *testing.T) {
	l := DefaultLimits()
	if l.PerRepo != 4 {
		t.Fatalf("expected PerRepo=4, got %d", l.PerRepo)
	}
}

func TestTrackerBasic(t *testing.T) {
	tr := NewTracker(DefaultLimits())
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		ok, err := tr.Acquire(ctx, "repo-1", "org-1")
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("expected acquire to succeed on iteration %d", i)
		}
	}
	ok, err := tr.Acquire(ctx, "repo-1", "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected 5th acquire to fail (per-repo limit)")
	}

	tr.Release("repo-1", "org-1")
	ok, err = tr.Acquire(ctx, "repo-1", "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected acquire to succeed after release")
	}
}

func TestTrackerOrgLimit(t *testing.T) {
	tr := NewTracker(Limits{PerRepo: 10, PerOrg: 3})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		ok, _ := tr.Acquire(ctx, fmt.Sprintf("repo-%d", i), "org-1")
		if !ok {
			t.Fatalf("expected acquire to succeed for repo-%d", i)
		}
	}
	ok, err := tr.Acquire(ctx, "repo-3", "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected 4th acquire in same org to fail (org limit)")
	}
}

func TestTrackerSystemLimit(t *testing.T) {
	tr := NewTracker(Limits{PerRepo: 10, System: 5})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		ok, _ := tr.Acquire(ctx, fmt.Sprintf("repo-%d", i), "org-1")
		if !ok {
			t.Fatalf("expected acquire to succeed for repo-%d", i)
		}
	}
	ok, err := tr.Acquire(ctx, "repo-5", "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected 6th acquire to fail (system limit)")
	}
}

func TestTrackerCount(t *testing.T) {
	tr := NewTracker(DefaultLimits())
	ctx := context.Background()

	tr.Acquire(ctx, "repo-1", "org-1")
	tr.Acquire(ctx, "repo-1", "org-1")
	tr.Acquire(ctx, "repo-2", "org-1")

	if tr.Count("repo-1") != 2 {
		t.Fatalf("expected 2 for repo-1, got %d", tr.Count("repo-1"))
	}
	if tr.Count("repo-2") != 1 {
		t.Fatalf("expected 1 for repo-2, got %d", tr.Count("repo-2"))
	}
	if tr.Count("nonexistent") != 0 {
		t.Fatalf("expected 0 for unknown repo, got %d", tr.Count("nonexistent"))
	}
	if tr.TotalCount() != 3 {
		t.Fatalf("expected total 3, got %d", tr.TotalCount())
	}
}

func TestLogFanOut(t *testing.T) {
	lf := NewLogFanOut(5)
	for i := 0; i < 5; i++ {
		if !lf.Acquire() {
			t.Fatalf("expected acquire to succeed on iteration %d", i)
		}
	}
	if lf.Acquire() {
		t.Fatal("expected 6th acquire to fail")
	}
	if lf.Active() != 5 {
		t.Fatalf("expected 5 active, got %d", lf.Active())
	}
	lf.Release()
	if lf.Active() != 4 {
		t.Fatalf("expected 4 active after release, got %d", lf.Active())
	}
	if !lf.Acquire() {
		t.Fatal("expected acquire to succeed after release")
	}
}

func TestLogFanOutUnlimited(t *testing.T) {
	lf := NewLogFanOut(0)
	for i := 0; i < 100; i++ {
		if !lf.Acquire() {
			t.Fatalf("expected unlimited acquire to succeed on iteration %d", i)
		}
	}
	if lf.Active() != 100 {
		t.Fatalf("expected 100 active, got %d", lf.Active())
	}
}

func TestTracker_ReleaseDoesNotGoNegative(t *testing.T) {
	tk := NewTracker(DefaultLimits())
	// Release without any Acquire — should not go negative
	tk.Release("repo", "org")
	if got := tk.Count("repo"); got != 0 {
		t.Fatalf("expected 0 after unmatched release, got %d", got)
	}
	if got := tk.TotalCount(); got != 0 {
		t.Fatalf("expected 0 total after unmatched release, got %d", got)
	}

	// Acquire then double-release — first release ok, second floored
	tk.Acquire(context.Background(), "repo", "org")
	tk.Release("repo", "org")
	tk.Release("repo", "org")
	if got := tk.Count("repo"); got != 0 {
		t.Fatalf("expected 0 after double release, got %d", got)
	}
}

func TestLogFanOut_ReleaseDoesNotGoNegative(t *testing.T) {
	l := NewLogFanOut(2)
	l.Release() // no acquire first
	if got := l.Active(); got != 0 {
		t.Fatalf("expected 0 after unmatched release, got %d", got)
	}
	_ = l.Acquire()
	l.Release()
	l.Release()
	if got := l.Active(); got != 0 {
		t.Fatalf("expected 0 after double release, got %d", got)
	}
}

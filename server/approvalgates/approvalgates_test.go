package approvalgates

import (
	"context"
	"testing"
	"time"

	"cicadoci.com/cicado/server/chat"
)

func TestOpenAndResolve(t *testing.T) {
	provider := chat.NewSlackProvider()
	resolved := make(chan GateState, 1)
	mgr := NewManager(provider, func(_ string, s GateState) {
		if s == GateApproved || s == GateDenied {
			resolved <- s
		}
	})

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy to production?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1", "user-2"},
		Timeout: time.Hour,
	}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatal(err)
	}
	if mgr.State("run-1") != GatePending {
		t.Fatal("expected GatePending")
	}

	if err := mgr.Resolve(context.Background(), "run-1", "user-1", "approve"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resolved:
		if got != GateApproved {
			t.Fatalf("expected GateApproved, got %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for resolution")
	}
}

func TestUnauthorizedActor(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)

	err := mgr.Resolve(context.Background(), "run-1", "user-evil", "approve")
	if err == nil {
		t.Fatal("expected error for unauthorized actor")
	}
	if mgr.State("run-1") != GatePending {
		t.Fatal("gate should still be pending after unauthorized attempt")
	}
}

func TestEmptyAllowList(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: nil,
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)

	err := mgr.Resolve(context.Background(), "run-1", "user-1", "approve")
	if err == nil {
		t.Fatal("expected error with empty allow-list (fail-closed)")
	}
}

func TestEmptyActorID(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)

	err := mgr.Resolve(context.Background(), "run-1", "", "approve")
	if err == nil {
		t.Fatal("expected error for empty actor ID")
	}
}

func TestDenyAction(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)

	if err := mgr.Resolve(context.Background(), "run-1", "user-1", "deny"); err != nil {
		t.Fatal(err)
	}
	if mgr.State("run-1") != GateDenied {
		t.Fatalf("expected GateDenied, got %v", mgr.State("run-1"))
	}
}

func TestIdempotentResolve(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)

	_ = mgr.Resolve(context.Background(), "run-1", "user-1", "approve")
	// Second resolve should be idempotent.
	err := mgr.Resolve(context.Background(), "run-1", "user-1", "approve")
	if err != nil {
		t.Fatalf("expected idempotent resolve, got %v", err)
	}
}

func TestCloseAllTimesOut(t *testing.T) {
	provider := chat.NewSlackProvider()
	resolved := make(chan GateState, 1)
	mgr := NewManager(provider, func(_ string, s GateState) {
		resolved <- s
	})

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)
	mgr.CloseAll("run-1")

	select {
	case got := <-resolved:
		if got != GateTimedOut {
			t.Fatalf("expected GateTimedOut, got %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestUnknownRunID(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)
	err := mgr.Resolve(context.Background(), "nonexistent", "user-1", "approve")
	if err == nil {
		t.Fatal("expected error for unknown run ID")
	}
}

func TestUnknownAction(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)

	err := mgr.Resolve(context.Background(), "run-1", "user-1", "unknown-action")
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
	if mgr.State("run-1") != GatePending {
		t.Fatal("gate should still be pending after unknown action")
	}
}

func TestResolveClosesButtons(t *testing.T) {
	// Catches the "CloseApproval silently fails" bug: without verifying
	// provider.Records() a wrong msgID would go undetected.
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
		Channel: "#ci",
	}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatal(err)
	}
	// After Open, the record should exist with Action = ActionApproval.
	records := provider.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].Action != chat.ActionApproval {
		t.Fatalf("expected ActionApproval, got %q", records[0].Action)
	}

	if err := mgr.Resolve(context.Background(), "run-1", "user-1", "approve"); err != nil {
		t.Fatal(err)
	}
	// After resolve, the record should have Action = ActionClose.
	records = provider.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 record after resolve, got %d", len(records))
	}
	if records[0].Action != chat.ActionClose {
		t.Fatalf("expected ActionClose after resolve, got %q — CloseApproval silently failed", records[0].Action)
	}
	if records[0].Status != chat.ApprovalApproved {
		t.Fatalf("expected ApprovalApproved status, got %v", records[0].Status)
	}
}

func TestCloseAllClosesButtons(t *testing.T) {
	// Catches the "CloseAll doesn't close the provider-side buttons" bug.
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-2",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
		Channel: "#ci",
	}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatal(err)
	}
	records := provider.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].Action != chat.ActionApproval {
		t.Fatalf("expected ActionApproval before CloseAll, got %q", records[0].Action)
	}

	mgr.CloseAll("run-2")
	records = provider.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 record after CloseAll, got %d", len(records))
	}
	if records[0].Action != chat.ActionClose {
		t.Fatalf("expected ActionClose after CloseAll, got %q — CloseAll didn't close buttons", records[0].Action)
	}
	if records[0].Status != chat.ApprovalTimedOut {
		t.Fatalf("expected ApprovalTimedOut status, got %v", records[0].Status)
	}
}

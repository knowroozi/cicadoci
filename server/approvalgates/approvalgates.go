// Package approvalgates implements approval gates as interactive chat
// buttons with per-user authorization (CIC-51).
//
// This is the killer feature for CICaDo's ChatOps UX. The critical
// security invariant: buttons are clickable by ANY channel member,
// so the handler that processes a button click MUST authorize the
// acting user against the gate's allow-list before honoring it —
// never against a channel default-bot identity. A button can be
// clicked by anyone in-channel, so the handler itself is the
// authorization gate.
package approvalgates

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cicadoci.com/cicado/server/chat"
)

// GateState is the state of an approval gate.
type GateState int

// Approval gate states.
const (
	// GatePending is waiting for an action.
	GatePending GateState = iota

	// GateApproved was approved.
	GateApproved

	// GateDenied was denied.
	GateDenied

	// GateTimedOut expired without action.
	GateTimedOut
)

// Gate is a single approval gate.
type Gate struct {
	// RunID is the run being gated.
	RunID string

	// Channel is the chat channel the gate message is posted to.
	Channel string

	// Reason is a human-readable reason.
	Reason string

	// Allow is the action ID that approves.
	Allow string

	// Deny is the action ID that denies.
	Deny string

	// Allowed is the allow-list of user IDs who can act.
	//
	// This is the security gate: a button click is authorized against
	// this list, not against a channel default.
	Allowed []string

	// Timeout is how long the gate stays open.
	Timeout time.Duration

	// MessageID is the chat message the buttons are rendered in.
	//
	// It is populated by Open from the provider's return value and
	// used by CloseApproval in Resolve.
	MessageID chat.MessageID
}

// Manager tracks approval gates and processes callbacks.
type Manager struct {
	mu       sync.Mutex
	gates    map[string]*GateRecord
	provider chat.ChatProvider
	// onResolved is called when a gate transitions to a terminal state.
	// Used to signal a waiter (e.g. a runner job blocked on approval).
	onResolved func(runID string, state GateState)
}

// GateRecord is the runtime record for an open gate.
type GateRecord struct {
	Gate      Gate
	State     GateState
	CreatedAt time.Time
	ResumedAt time.Time
	ActorID   string // who acted, for the audit trail
}

// NewManager creates a gate manager.
func NewManager(provider chat.ChatProvider, onResolved func(runID string, state GateState)) *Manager {
	return &Manager{
		gates:      map[string]*GateRecord{},
		provider:   provider,
		onResolved: onResolved,
	}
}

// Open registers a new gate and posts the approval buttons.
//
// After a successful Open, Gate.MessageID is populated with the
// message ID that Resolve/CloseApproval use to edit or close the
// live message.
//
// The msgID passed to RequestApproval is the same ID that
// CloseApproval later searches for. If the caller pre-set g.MessageID
// it is used directly; otherwise a deterministic ID is derived from
// g.RunID (one gate per run, so RunID is a unique key). Passing the
// requestID returned by RequestApproval would be wrong: RequestApproval
// in the stub stores MsgID from its arg, and the returned triggerID
// is a distinct concept (Slack trigger_id, used for modal-opening calls).
func (m *Manager) Open(ctx context.Context, g *Gate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := &GateRecord{
		Gate:      *g,
		State:     GatePending,
		CreatedAt: time.Now(),
	}
	m.gates[rec.Gate.RunID] = rec

	// Determine the message ID that this gate's buttons are posted on.
	// The caller may pre-set g.MessageID; otherwise derive it from RunID.
	msgID := string(g.MessageID)
	if msgID == "" {
		msgID = g.RunID
	}
	g.MessageID = chat.MessageID(msgID)

	_, _, err := m.provider.RequestApproval(ctx, g.Channel, msgID, chat.ApprovalRequest{
		RunID:        g.RunID,
		Reason:       g.Reason,
		Allow:        g.Allow,
		Deny:         g.Deny,
		TimeoutSec:   int(g.Timeout.Seconds()),
		AllowedUsers: g.Allowed,
	})
	if err != nil {
		return err
	}

	// Persist the resolved message ID on the internal record copy so that
	// Resolve/CloseAll can later look up the same message on the provider.
	rec.Gate.MessageID = g.MessageID
	return nil
}

// Resolve processes a button callback.
//
// The CRITICAL path: `actorID` is the user who clicked the button.
// We check them against the gate's allow-list. If they're not
// authorized, we reject (and the caller should audit the rejection).
func (m *Manager) Resolve(_ context.Context, runID, actorID, actionID string) error {
	m.mu.Lock()
	rec, ok := m.gates[runID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("approvalgates: gate %q not found", runID)
	}
	if rec.State != GatePending {
		m.mu.Unlock()
		// Idempotent: already resolved.
		return nil
	}
	// AUTHORIZATION: check the acting user against the allow-list.
	if !m.authorized(rec, actorID) {
		m.mu.Unlock()
		return fmt.Errorf("approvalgates: user %q not in allow-list for run %q", actorID, runID)
	}
	rec.ActorID = actorID
	rec.ResumedAt = time.Now()
	var status chat.ApprovalStatus
	switch actionID {
	case rec.Gate.Allow:
		rec.State = GateApproved
		status = chat.ApprovalApproved
	case rec.Gate.Deny:
		rec.State = GateDenied
		status = chat.ApprovalDenied
	default:
		m.mu.Unlock()
		return fmt.Errorf("approvalgates: unknown action %q", actionID)
	}
	// Copy the resolved gate for use outside the lock.
	gateCopy := *rec
	m.mu.Unlock()

	// Close the buttons so they can't be clicked again,
	// using the actual resolution status and the gate's channel/message.
	_ = m.provider.CloseApproval(context.Background(), gateCopy.Gate.Channel, string(gateCopy.Gate.MessageID), status)

	// Signal the waiter.
	if m.onResolved != nil {
		m.onResolved(runID, rec.State)
	}
	return nil
}

// authorized checks if the actor is in the gate's allow-list.
//
// This is THE security check. It runs on every Resolve. It must
// fail closed: if the allow-list is empty, no one is authorized.
func (m *Manager) authorized(rec *GateRecord, actorID string) bool {
	if actorID == "" {
		return false
	}
	for _, allowed := range rec.Gate.Allowed {
		if allowed == actorID {
			return true
		}
	}
	return false
}

// State returns the current state of a gate.
func (m *Manager) State(runID string) GateState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.gates[runID]; ok {
		return rec.State
	}
	return GatePending
}

// CloseAll marks all pending gates as resolved (e.g. on shutdown).
func (m *Manager) CloseAll(runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.gates[runID]
	if !ok || rec.State != GatePending {
		return
	}
	rec.State = GateTimedOut
	gateCopy := *rec
	// Close the provider-side unconditionally — a caller that passes no
	// onResolved callback still must not leave interactive buttons open.
	_ = m.provider.CloseApproval(context.Background(), gateCopy.Gate.Channel, string(gateCopy.Gate.MessageID), chat.ApprovalTimedOut)
	if m.onResolved != nil {
		m.onResolved(runID, rec.State)
	}
}

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
	//
	// A positive Timeout arms an automatic timeout: a gate that nobody
	// resolves transitions to GateTimedOut after this duration, so inaction
	// can never hold a run at GatePending forever. A zero Timeout means "no
	// automatic timeout" (the gate is resolved only by an explicit Resolve or
	// CloseAll) — the manager does not invent one.
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

	// timer, when non-nil, is the auto-timeout armed at Open for a gate whose
	// Timeout is set. It is stopped by any terminal transition (a manual Resolve
	// or CloseAll) so it can never resolve a gate that was resolved first.
	timer *time.Timer
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
//
// If the post fails the gate record is removed again: a half-open gate that
// was never actually posted to the channel must not linger in the manager,
// wait for a resolution that can never arrive, or leave a timer armed.
// (CIC-108 sub-bug: a gate was recorded even when the post failed, which
// left a dead record and a dangling callback.)
//
// A gate that carries a positive Timeout is armed with an auto-timeout, so
// inaction times it out on its own rather than sitting at GatePending forever
// the way the manager did before timeout enforcement was added (CIC-108).
func (m *Manager) Open(ctx context.Context, g *Gate) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// CIC-108 B1: a stale auto-timeout from an earlier Open for the same run
	// must never move a newer gate. Stop the previous record's timer before
	// overwriting it; the identity guard in onTimeout makes this safe even if a
	// callback is mid-flight.
	if old, ok := m.gates[g.RunID]; ok && old != nil && old.timer != nil {
		old.timer.Stop()
	}

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
		// No half-open gate: remove the record and do not arm a timer, so a post
		// that failed can never later be resolved, timed out, or close a message
		// that was never posted.
		delete(m.gates, rec.Gate.RunID)
		return err
	}

	// Persist the resolved message ID on the internal record copy so that
	// Resolve/CloseAll can later look up the same message on the provider.
	rec.Gate.MessageID = g.MessageID

	// Automatic timeout enforcement: arm a timer that moves a still-pending gate
	// to GateTimedOut after g.Timeout. Any manual resolution stops it, so the
	// timer is race-safe with Resolve/CloseAll via onTimeout's pending guard.
	// A zero Timeout means "no automatic timeout".
	if g.Timeout > 0 {
		// Capture rec by identity so a stale callback from an earlier Open for
		// the same run cannot move this record (CIC-108 B1).
		rec.timer = time.AfterFunc(g.Timeout, func() { m.onTimeout(rec) })
	}
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
	var status chat.ApprovalStatus
	// CIC-108/N3: the audit fields are assigned inside the terminal cases only, so
	// a click for an unknown action returns an error without recording that user
	// as the actor on a gate that is still pending.
	switch actionID {
	case rec.Gate.Allow:
		rec.ActorID = actorID
		rec.ResumedAt = time.Now()
		rec.State = GateApproved
		status = chat.ApprovalApproved
	case rec.Gate.Deny:
		rec.ActorID = actorID
		rec.ResumedAt = time.Now()
		rec.State = GateDenied
		status = chat.ApprovalDenied
	default:
		m.mu.Unlock()
		return fmt.Errorf("approvalgates: unknown action %q", actionID)
	}

	// Stop the auto-timeout now that we own the resolution, and copy the
	// terminal state out under the lock so the callback below reads only
	// by-value values. In practice the terminal-state write above is the only
	// writer of rec.State (it only happens on a pending->terminal transition),
	// so the pre-CIC-108 read-after-unlock was race-free in behavior; capturing
	// the by-value copy is still kept as defense-in-depth, not because a race
	// is reachable today.
	if rec.timer != nil {
		rec.timer.Stop()
	}
	gateCopy := *rec
	m.mu.Unlock()

	// Close the buttons so they can't be clicked again, using the actual
	// resolution status and the gate's channel/message.
	_ = m.provider.CloseApproval(context.Background(), gateCopy.Gate.Channel, string(gateCopy.Gate.MessageID), status)

	// Signal the waiter, OUTSIDE the lock and with the by-value state captured
	// above, so the callback cannot re-enter and deadlock on m.mu.
	if m.onResolved != nil {
		m.onResolved(runID, gateCopy.State)
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

// onTimeout moves a still-pending gate to GateTimedOut. It is the auto-timeout
// callback wired by Open and is also reachable in spirit from CloseAll; it is
// idempotent because it only acts on a gate still at GatePending, so it races
// safely with any manual Resolve/CloseAll (the lock serializes the guard).
func (m *Manager) onTimeout(rec *GateRecord) {
	m.mu.Lock()
	// CIC-108 B1: act only if this record is still the current gate for the run.
	// A later Open may have replaced it (and stopped this timer); a stray
	// callback from an earlier timer must not move the newer gate.
	cur, ok := m.gates[rec.Gate.RunID]
	if !ok || cur != rec || rec.State != GatePending {
		m.mu.Unlock()
		return
	}
	rec.State = GateTimedOut
	rec.ResumedAt = time.Now()
	if rec.timer != nil {
		rec.timer.Stop()
	}
	gateCopy := *rec
	m.mu.Unlock()

	// Close the timed-out buttons and signal the waiter outside the lock, with
	// the by-value state captured above.
	_ = m.provider.CloseApproval(context.Background(), gateCopy.Gate.Channel, string(gateCopy.Gate.MessageID), chat.ApprovalTimedOut)
	if m.onResolved != nil {
		m.onResolved(rec.Gate.RunID, gateCopy.State)
	}
}

// CloseAll resolves a single still-pending gate as a timeout (e.g. on
// shutdown). It is deliberately named for the shutdown use case in which it is
// the manager's way of clearing the gate; it accepts the one runID being closed.
//
// The terminal transition and the value copies happen under the lock; the
// provider close and the onResolved callback happen OUTSIDE it, so a callback
// that re-enters the manager (e.g. to read State) cannot deadlock on m.mu.
// (CIC-108 sub-bug: onResolved was previously called while the lock was held.)
func (m *Manager) CloseAll(runID string) {
	m.mu.Lock()
	rec, ok := m.gates[runID]
	if !ok || rec.State != GatePending {
		m.mu.Unlock()
		return
	}
	rec.State = GateTimedOut
	rec.ResumedAt = time.Now()
	if rec.timer != nil {
		rec.timer.Stop()
	}
	gateCopy := *rec
	m.mu.Unlock()

	// Close the provider-side unconditionally — a caller that passes no onResolved
	// callback still must not leave interactive buttons open.
	_ = m.provider.CloseApproval(context.Background(), gateCopy.Gate.Channel, string(gateCopy.Gate.MessageID), chat.ApprovalTimedOut)
	if m.onResolved != nil {
		m.onResolved(runID, gateCopy.State)
	}
}

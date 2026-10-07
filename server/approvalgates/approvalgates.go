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
	mu    sync.Mutex
	gates map[string]*GateRecord
	// provider is the chat provider used to render and close gates.
	provider chat.ChatProvider
	// onResolved is called when a gate transitions to a terminal state. It
	// signals a waiter (e.g. a runner job blocked on approval). The third
	// by-value argument is the reason for the terminal transition so a
	// caller -- including the drain-on-shutdown drain in main.go -- can
	// distinguish a normal timeout or denial from a shutdown drain, whose
	// audit reason must not read "approval timed out" (CIC-204 B1).
	onResolved func(runID string, state GateState, reason string)
	// closed is set to true by DrainAll. Once set, a new Open is refused
	// with an error so a late-arriving webhook cannot open a gate after the
	// shutdown drain has run (CIC-204 O1).
	closed bool
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

	// signal is closed exactly once when the gate reaches a terminal state.
	// Await selects on it to unblock a goroutine that should stop holding
	// jobs pending.
	signal chan struct{}

	// terminalReason is the machine-readable reason a gate transitioned
	// to terminal -- set by onTimeout (ReasonApprovalTimedOut) and
	// closeOne (the caller-supplied reason, e.g. ReasonShutdownDrain).
	// Resolve never sets it; a user-driven transition keeps it empty and
	// the controller falls back to a state-based default.
	// Empty while pending.
	terminalReason string
}

// ReasonApprovalTimedOut is the terminal reason a gate reaches GateTimedOut when
// its own auto-timeout fires. It is the default for the onTimeout and CloseAll
// paths.
const ReasonApprovalTimedOut = "approval timed out"

// ReasonShutdownDrain is the terminal reason a gate reaches GateTimedOut when
// DrainAll closes it at process shutdown. It is distinct from
// ReasonApprovalTimedOut so the run's failure audit row can say
// "abandoned: server shutdown" instead of the misleading "approval timed out"
// that a restart would otherwise produce (CIC-204 B1).
const ReasonShutdownDrain = "abandoned: server shutdown"

// ReasonRunCancelled is the terminal reason a still-pending gate that
// belongs to a cancelled run is moved to GateTimedOut. CIC-208 F2:
// the cancel-side close must not read as a timeout.
const ReasonRunCancelled = "cancelled: run cancelled"

// ReasonNoJobs is the terminal reason recorded when a zero-job run's gate
// is closed (CIC-209). It covers both the no-match case (no workflow in the
// repo matched the event) and the load-failure case (a matching workflow
// failed to build or create jobs) — in both cases the run ends up with
// zero jobs, so the gate has no jobs to dispatch. The run's own terminal
// status (failed on load error, succeeded otherwise) is set separately by
// finishIfNoJobs; this constant names the gate close, not the run status.
const ReasonNoJobs = "zero jobs: run had no matching workflow"

// NewManager creates a gate manager.
func NewManager(provider chat.ChatProvider, onResolved func(runID string, state GateState, reason string)) *Manager {
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

	// A manager that DrainAll has closed must refuse to open new gates. A
	// late-arriving webhook that races the drain would otherwise open a gate
	// that never gets closed or audited -- the exact "neither audited nor
	// closed" failure CIC-204 addresses (O1).
	if m.closed {
		return fmt.Errorf("approvalgates: manager is closed (shutdown drain in progress) -- refusing new gate for run %q", g.RunID)
	}

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
		signal:    make(chan struct{}),
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
	close(rec.signal)

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
	// above, so the callback cannot re-enter and deadlock on m.mu. A user
	// resolve carries an empty reason; a shutdown drain carries
	// ReasonShutdownDrain so the caller (the controller's dispatchAfterGate)
	// can write the right audit reason (CIC-204 B1).
	if m.onResolved != nil {
		m.onResolved(runID, gateCopy.State, "")
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

// ActorID returns the user who resolved the run's gate, or "" when the gate
// is unknown, still pending, or timed out. onResolved callbacks run outside
// the lock, so they may call it to attribute the audit entry (CIC-162).
func (m *Manager) ActorID(runID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.gates[runID]; ok {
		return rec.ActorID
	}
	return ""
}

// Await blocks until the gate for runID reaches a terminal state, then
// returns that state and its terminal reason. It returns GatePending with
// reason "" (immediately) when the runID is not yet open.
//
// When ctx is cancelled before the gate resolves, Await returns
// GateTimedOut with reason "" so the caller can fail the run rather
// than hang -- the empty reason is the caller's cue that this was a
// cancel-driven timeout, not a real auto-timeout or a drain.
func (m *Manager) Await(ctx context.Context, runID string) (GateState, string) {
	m.mu.Lock()
	rec, ok := m.gates[runID]
	if !ok {
		m.mu.Unlock()
		return GatePending, ""
	}
	// Copy the signal channel (it is never replaced).
	sig := rec.signal
	state := rec.State
	reason := rec.terminalReason
	m.mu.Unlock()

	if state != GatePending {
		return state, reason
	}
	select {
	case <-sig:
		m.mu.Lock()
		defer m.mu.Unlock()
		if cur, ok := m.gates[runID]; ok {
			return cur.State, cur.terminalReason
		}
		return state, reason
	case <-ctx.Done():
		return GateTimedOut, ""
	}
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
	rec.terminalReason = ReasonApprovalTimedOut
	if rec.timer != nil {
		rec.timer.Stop()
	}
	close(rec.signal)
	gateCopy := *rec
	m.mu.Unlock()

	// Close the timed-out buttons and signal the waiter outside the lock, with
	// the by-value state captured above.
	_ = m.provider.CloseApproval(context.Background(), gateCopy.Gate.Channel, string(gateCopy.Gate.MessageID), chat.ApprovalTimedOut)
	if m.onResolved != nil {
		m.onResolved(rec.Gate.RunID, gateCopy.State, ReasonApprovalTimedOut)
	}
}

// CloseAll resolves a single still-pending gate as a normal timeout. It is
// called by the controller's load-failure paths (controller.go:363 and :506)
// when an approval run fails to load: a gate that was already open for the
// failing run must be closed. The shutdown drain lives in DrainAll, which is
// distinct -- it tags the transition with ReasonShutdownDrain and sets the
// manager's closed flag so late opens are refused.
//
// The terminal transition and the value copies happen under the lock; the
// provider close and the onResolved callback happen OUTSIDE it, so a callback
// that re-enters the manager (e.g. to read State) cannot deadlock on m.mu.
// (CIC-108 sub-bug: onResolved was previously called while the lock was held.)
// closeOne is the shared per-gate terminal step. It moves a still-pending
// gate to GateTimedOut using the supplied terminal reason; it is used by
// CloseAll (the load-failure path) and by DrainAll (the shutdown path) so
// their terminal-side-effect pattern is shared.
//
// The state-guard makes it idempotent: a timer that fires after closeOne has
// already moved the gate is a no-op. A timer that fires AFTER closeOne moved
// the gate sees the terminal state and is a no-op, which is why the
// no-double-audit test can exercise the guard directly.
//
// The terminal transition happens under the lock; the provider close and the
// onResolved callback happen OUTSIDE it, so a callback that re-enters the
// manager cannot deadlock.
func (m *Manager) closeOne(runID, reason string) {
	m.mu.Lock()
	rec, ok := m.gates[runID]
	if !ok || rec.State != GatePending {
		m.mu.Unlock()
		return
	}
	rec.State = GateTimedOut
	rec.terminalReason = reason
	rec.ResumedAt = time.Now()
	if rec.timer != nil {
		rec.timer.Stop()
	}
	close(rec.signal)
	gateCopy := *rec
	m.mu.Unlock()

	// Close the provider-side unconditionally -- a caller that passes no
	// onResolved callback still must not leave interactive buttons open.
	_ = m.provider.CloseApproval(context.Background(), gateCopy.Gate.Channel, string(gateCopy.Gate.MessageID), chat.ApprovalTimedOut)
	if m.onResolved != nil {
		m.onResolved(runID, gateCopy.State, reason)
	}
}

// CloseAll moves a still-pending gate to GateTimedOut using the load-failure
// reason. Its callers are the controller's load-failure paths
// (controller.go:363 and controller.go:506) -- the terminal-side-effect
// pattern is shared with the shutdown drain via closeOne.
//
// CIC-204 D1 reword: the doc used to say "named for the shutdown use
// case", which was wrong -- the shutdown drain is DrainAll, not CloseAll.
func (m *Manager) CloseAll(runID string) {
	m.closeOne(runID, ReasonApprovalTimedOut)
}

// DrainAll resolves every still-pending gate as a timed-out. It is the
// shutdown drain (CIC-204): a gate nobody acted on before the process exits
// must be moved to a terminal state, its interactive buttons closed, and its
// transition audited -- otherwise it is "neither audited nor closed" and the
// run it holds is abandoned with no record. It delegates to CloseAll per runID
// so the terminal-side-effect pattern (provider close + onResolved outside the
// lock, with the pending-idempotency guard) is shared. It is race-safe against a
// gate's own auto-timeout: both CloseAll and onTimeout move a record only while
// it is still GatePending, so a timer that fires after the drain is a no-op.
//
// The snapshot-under-lock-then-close pattern is correct because at shutdown
// nothing opens a new gate. If ctx is done it stops draining early; whatever
// was not drained is abandoned by process exit exactly as it was before this
// method existed, so the partial drain is never worse than no drain. Returns the
// number of gates it drained.
func (m *Manager) DrainAll(ctx context.Context) int {
	m.mu.Lock()
	pending := make([]string, 0, len(m.gates))
	for runID, rec := range m.gates {
		if rec.State == GatePending {
			pending = append(pending, runID)
		}
	}
	// Set closed under the same lock as the snapshot so the drain is
	// atomic with respect to a concurrent Open (which also holds m.mu).
	// A late Open is refused once closed is true (CIC-204 O1).
	m.closed = true
	m.mu.Unlock()

	drained := 0
	for _, runID := range pending {
		if ctx.Err() != nil {
			break
		}
		m.closeOne(runID, ReasonShutdownDrain)
		drained++
	}
	return drained
}

// String returns a human-readable name for the gate state.
func (s GateState) String() string {
	switch s {
	case GateApproved:
		return "approved"
	case GateDenied:
		return "denied"
	case GateTimedOut:
		return "timed_out"
	case GatePending:
		return "pending"
	default:
		return fmt.Sprintf("GateState(%d)", int(s))
	}
}

// CancelGate closes a still-pending gate for a run that has been cancelled.
// It is the cancel-side counterpart to CloseAll (load-failure/shutdown):
// the ticket is CIC-208 — a cancellation must not leave a dangling gate
// record (timer + map entry) that nobody resolves.
//
// It delegates to CloseAll, which already handles the terminal transition,
// provider-side button close, and onResolved signal. The distinct name
// documents intent at the call site: when a run is cancelled, its gate
// must be closed, even if no one resolved it.
func (m *Manager) CancelGate(runID string) {
	m.closeOne(runID, ReasonRunCancelled)
}

// CloseNoJobs closes a gate that was opened for a zero-job run (CIC-209),
// using the ReasonNoJobs terminal reason so audit and Slack messaging
// are correct.
func (m *Manager) CloseNoJobs(runID string) {
	m.closeOne(runID, ReasonNoJobs)
}

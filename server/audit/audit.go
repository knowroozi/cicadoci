// Package audit implements CICaDo's audit trail (CIC-41).
//
// Every state-changing action (approve, cancel, rerun, deploy)
// is logged as a structured, append-only event. The audit log
// is the system of record for "who did what and when."
//
// In production, entries are written to a WORM-compliant store.
// The MVP uses an in-memory ring buffer with a hash chain for
// basic tamper detection.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Action is a state-changing action that must be audited.
type Action string

// Known actions.
const (
	ActionApprove Action = "approve"
	// ActionDeny records an approval gate denied by an authorized user (CIC-162).
	ActionDeny Action = "deny"
	// ActionGateTimeout records an approval gate that timed out or was closed
	// unresolved (CIC-162).
	ActionGateTimeout  Action = "gate_timeout"
	ActionCancel       Action = "cancel"
	ActionRerun        Action = "rerun"
	ActionDeploy       Action = "deploy"
	ActionRollback     Action = "rollback"
	ActionCreateRun    Action = "create_run"
	ActionSecretPut    Action = "secret.put"
	ActionSecretDelete Action = "secret.delete"
)

// Principal-type and surface labels, as valid in the audit_events CHECK sets
// (see server/store/migrations/0001_init.sql). Named constants keep callers in
// sync with those sets and avoid scattered magic strings.
const (
	// Principal labels (principal_type CHECK: user, runner, agent, token, system).
	PrincipalUser   = "user"
	PrincipalSystem = "system"
	PrincipalRunner = "runner"
	PrincipalAgent  = "agent"
	PrincipalToken  = "token"

	// Surface labels (surface CHECK: web, chat, mcp, api, forge, system).
	SurfaceWeb    = "web"
	SurfaceChat   = "chat"
	SurfaceMCP    = "mcp"
	SurfaceAPI    = "api"
	SurfaceForge  = "forge"
	SurfaceSystem = "system"
)

// Entry is a single audit log entry.
type Entry struct {
	// HashVersion tags the hash algorithm:
	//    1 = legacy hash (6 fields: action, actor, target, reason,
	//           timestamp, prev_hash)
	//    2 = extends v1 with principal_type, surface, org_id
	//
	// CIC-205: Append writes new rows with v2. VerifyChain dispatches
	// on this per-row field.
	HashVersion int

	// ID is a monotonic sequence number.
	ID int64

	// Action is the type of action.
	Action Action

	// Actor is the user (or system) that performed the action.
	Actor string

	// Target is what the action was applied to.
	Target string

	// PrincipalType is the kind of principal that performed the action: one of
	// user, runner, agent, token, or system (the audit_events.principal_type
	// CHECK set). PostgresLog stores it verbatim; an empty value defaults to
	// "user" — the historical default, so callers that do not set the field
	// keep their previous behaviour.
	//
	// CIC-192: this field existed nowhere before, so the PostgresLog hardcoded
	// 'user' for every row, and a system-initiated event (e.g. a gate timeout)
	// was recorded as a user event. Callers must set it to reflect reality.
	PrincipalType string

	// OrgID is the UUID of the org the principal belongs to, or the empty
	// string for "no org" (written as NULL by PostgresLog; the column is uuid
	// with ON DELETE SET NULL). CIC-192: the column is already nullable, so
	// leaving this unset preserves the previous (always-NULL) behaviour.
	OrgID string

	// Surface is the ingress surface that originated the action: one of web,
	// chat, mcp, api, forge, system (the audit_events.surface CHECK set).
	// PostgresLog stores it verbatim; an empty value defaults to "system" — the
	// historical default.
	//
	// CIC-192: this field existed nowhere before, so the PostgresLog hardcoded
	// 'system' for every row, and a chat- or api-originated event was recorded
	// as a system event. Callers must set it to reflect the real surface.
	Surface string

	// Reason is a human-readable reason.
	Reason string

	// Timestamp is when the action occurred.
	Timestamp time.Time

	// Hash is the SHA-256 of the serialized entry.
	Hash string

	// PrevHash is the hash of the previous entry (hash chain).
	PrevHash string
}

// Log is an append-only audit log.
type Log interface {
	// Append records a new audit entry.
	Append(ctx context.Context, e Entry) (int64, error)

	// Get returns an entry by ID.
	Get(ctx context.Context, id int64) (Entry, error)

	// List returns entries after a given ID.
	List(ctx context.Context, afterID int64, limit int) ([]Entry, error)

	// VerifyChain checks that the hash chain is intact.
	VerifyChain(ctx context.Context) error
}

// MemoryLog is an in-memory audit log for testing.
type MemoryLog struct {
	mu      sync.RWMutex
	entries []Entry
	nextID  int64
}

// NewMemoryLog creates a new in-memory audit log.
//
// A genesis entry with ID=0 is automatically added.
func NewMemoryLog() *MemoryLog {
	genesis := Entry{
		ID:        0,
		Action:    Action(""),
		Timestamp: time.Now(),
		Hash:      "0000000000000000000000000000000000000000000000000000000000000000",
	}
	return &MemoryLog{
		entries: []Entry{genesis},
		nextID:  1,
	}
}

// hashEntry computes the hash for an entry.
//
// ID is deliberately excluded from the hashed struct: Postgres assigns
// the id via bigserial and it is unknowable at hash-time. MemoryLog can
// include it because it assigns IDs before hashing, but that backend-
// specific behaviour must not leak into a backend-agnostic hash function.
// Chain integrity is maintained by the prev_hash pointer, not by the id.
// hashEntry computes the v1 hash (legacy, 6 fields).
func hashEntry(e Entry) string {
	body, _ := json.Marshal(struct {
		Action    Action
		Actor     string
		Target    string
		Reason    string
		Timestamp time.Time
		PrevHash  string
	}{
		Action:    e.Action,
		Actor:     e.Actor,
		Target:    e.Target,
		Reason:    e.Reason,
		Timestamp: e.Timestamp,
		PrevHash:  e.PrevHash,
	})
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// hashEntryV2 computes the v2 hash (CIC-205): v1 fields plus
// principal_type, surface, org_id. Rows written after CIC-205
// use this.
func hashEntryV2(e Entry) string {
	body, _ := json.Marshal(struct {
		Action        Action
		Actor         string
		Target        string
		Reason        string
		Timestamp     time.Time
		PrevHash      string
		PrincipalType string
		Surface       string
		OrgID         string
	}{
		Action:        e.Action,
		Actor:         e.Actor,
		Target:        e.Target,
		Reason:        e.Reason,
		Timestamp:     e.Timestamp.UTC(),
		PrevHash:      e.PrevHash,
		PrincipalType: e.PrincipalType,
		Surface:       e.Surface,
		OrgID:         e.OrgID,
	})
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// hashForRow dispatches on HashVersion:
//
//	v2 uses hashEntryV2; anything else uses the v1 hash.
func hashForRow(e Entry) string {
	if e.HashVersion == 2 {
		return hashEntryV2(e)
	}
	return hashEntry(e)
}

// Append records a new audit entry.
func (m *MemoryLog) Append(_ context.Context, e Entry) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := m.nextID
	m.nextID++

	e.ID = id
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	e.HashVersion = 2 // CIC-205: new rows are v2
	// Set PrevHash to the last entry's hash.
	e.PrevHash = m.entries[len(m.entries)-1].Hash
	e.Hash = hashForRow(e)

	m.entries = append(m.entries, e)
	return id, nil
}

// Get returns an entry by ID.
func (m *MemoryLog) Get(_ context.Context, id int64) (Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if id < 0 || int(id) >= len(m.entries) {
		return Entry{}, fmt.Errorf("audit: entry %d not found", id)
	}
	return m.entries[id], nil
}

// List returns entries after a given ID.
func (m *MemoryLog) List(_ context.Context, afterID int64, limit int) ([]Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	start := int(afterID) + 1
	if start >= len(m.entries) {
		return nil, nil
	}
	end := len(m.entries)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	out := make([]Entry, end-start)
	copy(out, m.entries[start:end])
	return out, nil
}

// VerifyChain checks that the hash chain is intact.
func (m *MemoryLog) VerifyChain(_ context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for i, e := range m.entries {
		if i == 0 {
			continue // genesis entry
		}
		if e.PrevHash != m.entries[i-1].Hash {
			return fmt.Errorf("audit: hash chain broken at entry %d: PrevHash=%s != prev.Hash=%s",
				i, e.PrevHash, m.entries[i-1].Hash)
		}
		if hashForRow(e) != e.Hash {
			return fmt.Errorf("audit: hash mismatch at entry %d: computed=%s stored=%s",
				i, hashForRow(e), e.Hash)
		}
	}
	return nil
}

// Len returns the number of entries (including genesis).
func (m *MemoryLog) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

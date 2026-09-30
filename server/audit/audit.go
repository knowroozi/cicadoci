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
	ActionApprove      Action = "approve"
	ActionCancel       Action = "cancel"
	ActionRerun        Action = "rerun"
	ActionDeploy       Action = "deploy"
	ActionRollback     Action = "rollback"
	ActionCreateRun    Action = "create_run"
	ActionSecretPut    Action = "secret.put"
	ActionSecretDelete Action = "secret.delete"
)

// Entry is a single audit log entry.
type Entry struct {
	// ID is a monotonic sequence number.
	ID int64

	// Action is the type of action.
	Action Action

	// Actor is the user (or system) that performed the action.
	Actor string

	// Target is what the action was applied to.
	Target string

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
	// Set PrevHash to the last entry's hash.
	e.PrevHash = m.entries[len(m.entries)-1].Hash
	e.Hash = hashEntry(e)

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
		if hashEntry(e) != e.Hash {
			return fmt.Errorf("audit: hash mismatch at entry %d: computed=%s stored=%s",
				i, hashEntry(e), e.Hash)
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

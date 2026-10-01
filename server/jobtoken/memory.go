package jobtoken

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Memory is a concurrency-safe, in-memory Store for tests and for a
// single-process deployment. It is the default backend for the
// unit tests; production uses Postgres.
//
// The store keeps only the hashed form. The raw bearer is
// never persisted: a leak of the store (e.g. a core dump in
// single-process mode) does not expose the token.
type Memory struct {
	mu sync.RWMutex
	by map[[32]byte]record // keyed by SHA-256 of the raw bearer
}

// NewMemory creates an empty in-memory store.
func NewMemory() *Memory { return &Memory{by: make(map[[32]byte]record)} }

// Mint stores the hashed token and returns its ID.
func (m *Memory) Mint(_ context.Context, raw string, c Claims, expiry time.Time) (uuid.UUID, error) {
	h := sha256.Sum256([]byte(raw))
	t := newToken(h[:], c, expiry)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.by[h] = t
	return t.ID, nil
}

// Lookup returns the record matching the hash, or ErrStoreNotFound.
//
// The hash is compared via a constant-time key lookup on the map —
// SHA-256 hash equality is a random 256-bit value, so a map lookup
// is already effectively constant-time with respect to the token
// itself; only the map key is being compared, which is the SHA-256
// output, not the raw bytes. Any leak through the raw bytes would
// still be bounded by the SHA-256 collision probability, so a
// map lookup is acceptable.
func (m *Memory) Lookup(_ context.Context, hash []byte) (record, error) {
	if len(hash) != 32 {
		return record{}, ErrStoreNotFound
	}
	var h [32]byte
	copy(h[:], hash)
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.by[h]
	if !ok {
		return record{}, ErrStoreNotFound
	}
	return rec, nil
}

// MarkRevoked sets RevokedAt on the record matching id, only if it is
// not already revoked. Returns changed=true when a transition actually
// happened; a no-op for an unknown id or an already-revoked record
// returns (false, nil).
func (m *Memory) MarkRevoked(_ context.Context, id uuid.UUID, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, rec := range m.by {
		if rec.ID == id && rec.RevokedAt == nil {
			r := rec
			r.RevokedAt = &at
			m.by[k] = r
			return true, nil
		}
	}
	return false, nil
}

// SetLastUse updates last-use for the matching record. It is a
// no-op for the Memory store: last-use is a Postgres-column
// nicety, not a security load-bearing field.
func (m *Memory) SetLastUse(_ context.Context, _ uuid.UUID, _ time.Time) error {
	return nil
}

// Len returns the number of stored tokens.
func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.by)
}

// newToken builds a record with a freshly-generated ID.
func newToken(hash []byte, claims Claims, expiry time.Time) record {
	h := make([]byte, 32)
	copy(h, hash)
	return record{
		ID:     uuid.New(),
		Hash:   h,
		Claims: claims,
		Expiry: expiry,
	}
}

// Ensure Memory satisfies Store.
var _ Store = (*Memory)(nil)

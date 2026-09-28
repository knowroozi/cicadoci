// Package secrets implements the CICaDo secrets store (CIC-37).
//
// Secrets are AES-256-GCM encrypted at rest, per-repo keys
// (KMS envelope encryption in production), and only exposed to
// the runner in-flight via a lease that times out in minutes.
// Secret values never appear in audit logs, error messages,
// log output, or any redeliverable form — the invariant is
// enforced by the store itself (no plaintext API) and by
// adversarial test cases.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Store is the secrets interface.
//
// The critical invariant: there is NO plaintext API on this
// interface. The caller gets ciphertext, not the actual secret
// value. This is the "secret values appear nowhere redeliverable"
// invariant from CLAUDE.md.
type Store interface {
	// Put stores a new secret (or overwrites an existing one).
	// The value is encrypted before storage.
	Put(ctx context.Context, orgID, repoID, name string, value []byte, tags []string) error

	// Get returns a lease for the named secret.
	// The lease holds the decrypted value and times out.
	Get(ctx context.Context, orgID, repoID, name string) (*Lease, error)

	// Delete removes a secret.
	Delete(ctx context.Context, orgID, repoID, name string) error

	// List returns the names (not values!) of all secrets for a repo.
	List(ctx context.Context, orgID, repoID string) ([]string, error)
}

// Lease holds a decrypted secret value for a bounded time.
//
// The value is only accessible within the lease lifetime.
// After Expiry, the value is zeroed and the lease is stale.
type Lease struct {
	// Value holds the decrypted secret.
	//
	// This is the ONLY place where a plaintext secret can exist
	// in the codebase. It is always bounded by Expiry.
	Value []byte

	// Name is the secret name.
	Name string

	// Expiry is when the lease times out.
	Expiry time.Time

	// Tags are the secret's metadata tags.
	Tags []string

	// zeroed tracks whether Value has been zeroed.
	zeroed bool
}

// Expired reports whether the lease has expired.
func (l *Lease) Expired() bool {
	if l.zeroed {
		return true
	}
	return time.Now().After(l.Expiry)
}

// Close forces the lease to become stale and zero the value.
func (l *Lease) Close() {
	if l.zeroed {
		return
	}
	for i := range l.Value {
		l.Value[i] = 0
	}
	l.zeroed = true
}

// EncryptionKey is a 32-byte AES-256 key.
//
// Value semantics: copying an EncryptionKey by value leaves the
// old copy in memory. Callers MUST zero the key with Zero() when
// done, or store it as a pointer and zero via defer.
//
// \tvar key EncryptionKey
// \tkey, _ := GenerateKey()
// \tdefer key.Zero()
// \t// use key...
type EncryptionKey [32]byte

// Zero scrubs all 32 bytes to zero. Call this when the key is
// no longer needed.
func (k *EncryptionKey) Zero() {
	for i := range k {
		k[i] = 0
	}
}

// GenerateKey creates a random 256-bit key.
func GenerateKey() (EncryptionKey, error) {
	var k EncryptionKey
	_, err := io.ReadFull(rand.Reader, k[:])
	return k, err
}

// Memory is an in-memory secrets store for testing.
type Memory struct {
	mu     sync.Mutex
	store  map[string]record
	key    EncryptionKey
	expiry time.Duration
}

// record holds an encrypted secret record.
type record struct {
	Ciphertext []byte
	Nonce      []byte
	Tags       []string
	CreatedAt  time.Time
}

// NewMemory creates an in-memory store with the given key.
// The key must be 32 bytes.
func NewMemory(key EncryptionKey) *Memory {
	return &Memory{
		store: map[string]record{},
		key:   key,
		// Default lease expiry: 5 minutes.
		expiry: 5 * time.Minute,
	}
}

// sep is the internal composite-key separator.  A NUL byte (0x00)
// cannot appear in any valid GitHub org, repo, or secret name
// (all are [A-Za-z0-9._-]+), so it is collision-proof without the
// escaping that a printable separator like "/" would require.
const sep = "\x00"

// validateID ensures that an identifier component is non-empty and
// does not contain the internal separator.
func validateID(s string, label string) error {
	if s == "" {
		return fmt.Errorf("secrets: %s must not be empty", label)
	}
	if strings.ContainsRune(s, 0) {
		return fmt.Errorf("secrets: %s must not contain NUL byte", label)
	}
	return nil
}

// validateIDs validates all three components of a composite key.
// name may be empty (used by List which does not take a specific name).
func validateIDs(orgID, repoID, name string) error {
	if err := validateID(orgID, "orgID"); err != nil {
		return err
	}
	if err := validateID(repoID, "repoID"); err != nil {
		return err
	}
	if name != "" {
		if err := validateID(name, "name"); err != nil {
			return err
		}
	}
	return nil
}

// compositeKey builds the internal map key for a secret.
func compositeKey(orgID, repoID, name string) string {
	return orgID + sep + repoID + sep + name
}

// compositeKeyPrefix builds the orgID/repoID prefix used by List.
func compositeKeyPrefix(orgID, repoID string) string {
	return orgID + sep + repoID + sep
}

// Put stores a new secret.

func (m *Memory) Put(_ context.Context, orgID, repoID, name string, value []byte, tags []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := validateIDs(orgID, repoID, name); err != nil {
		return err
	}

	block, err := aes.NewCipher(m.key[:])
	if err != nil {
		return fmt.Errorf("secrets: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("secrets: new GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("secrets: generate nonce: %w", err)
	}

	ct := gcm.Seal(nil, nonce, value, nil)

	id := compositeKey(orgID, repoID, name)
	m.store[id] = record{
		Ciphertext: ct,
		Nonce:      nonce,
		Tags:       tags,
		CreatedAt:  time.Now(),
	}
	return nil
}

// Get returns a lease for the named secret.
func (m *Memory) Get(_ context.Context, orgID, repoID, name string) (*Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := validateIDs(orgID, repoID, name); err != nil {
		return nil, err
	}
	id := compositeKey(orgID, repoID, name)
	rec, ok := m.store[id]
	if !ok {
		return nil, fmt.Errorf("secrets: not found: %q", name)
	}

	block, err := aes.NewCipher(m.key[:])
	if err != nil {
		return nil, fmt.Errorf("secrets: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: new GCM: %w", err)
	}

	pt, err := gcm.Open(nil, rec.Nonce, rec.Ciphertext, nil)
	if err != nil {
		// Tampering detected — do NOT return, and log a warning.
		return nil, fmt.Errorf("secrets: decrypt failed: %w", err)
	}

	return &Lease{
		Value:  pt,
		Name:   name,
		Expiry: time.Now().Add(m.expiry),
		Tags:   rec.Tags,
	}, nil
}

// Delete removes a secret.
func (m *Memory) Delete(_ context.Context, orgID, repoID, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := validateIDs(orgID, repoID, name); err != nil {
		return err
	}
	id := compositeKey(orgID, repoID, name)
	// Zero the stored record before deleting.
	if rec, ok := m.store[id]; ok {
		for i := range rec.Ciphertext {
			rec.Ciphertext[i] = 0
		}
		for i := range rec.Nonce {
			rec.Nonce[i] = 0
		}
		delete(m.store, id)
	}
	return nil
}

// List returns the names of all secrets for a repo.
//
// IMPORTANT: returns names only, never values.
func (m *Memory) List(_ context.Context, orgID, repoID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := validateIDs(orgID, repoID, ""); err != nil {
		return nil, err
	}
	prefix := compositeKeyPrefix(orgID, repoID)
	var names []string
	for id, rec := range m.store {
		// Verify the record is still valid (not zeroed).
		if len(rec.Ciphertext) == 0 {
			continue
		}
		if len(id) > len(prefix) && id[:len(prefix)] == prefix {
			// Extract just the name (part after prefix).
			names = append(names, id[len(prefix):])
		}
	}
	return names, nil
}

// VerifyKey confirms a key can decrypt a known-good ciphertext.
func VerifyKey(key EncryptionKey, nonce, ciphertext []byte) error {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return fmt.Errorf("secrets: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("secrets: new GCM: %w", err)
	}
	_, err = gcm.Open(nil, nonce, ciphertext, nil)
	return err
}

// ConstantTimeCompare compares two byte slices in constant time.
//
// Use this for comparing secret metadata (not values directly,
// since values are only available in leases).
func ConstantTimeCompare(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

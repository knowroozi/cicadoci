package artifacts

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Memory is an in-memory Store for testing. It is safe for concurrent use.
type Memory struct {
	mu      sync.RWMutex
	byID    map[uuid.UUID]Artifact
	byRunID map[uuid.UUID][]uuid.UUID // runID → ordered list of artifact IDs
}

// NewMemory returns an empty Memory Store.
func NewMemory() *Memory {
	return &Memory{
		byID:    make(map[uuid.UUID]Artifact),
		byRunID: make(map[uuid.UUID][]uuid.UUID),
	}
}

// Create stores an artifact.
func (m *Memory) Create(_ context.Context, a Artifact) (Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	m.byID[a.ID] = a
	m.byRunID[a.RunID] = append(m.byRunID[a.RunID], a.ID)
	return a, nil
}

// GetByRunID returns all artifacts for a run.
func (m *Memory) GetByRunID(_ context.Context, runID uuid.UUID) ([]Artifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := m.byRunID[runID]
	out := make([]Artifact, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.byID[id])
	}
	return out, nil
}

// GetByID returns a single artifact by ID.
func (m *Memory) GetByID(_ context.Context, id uuid.UUID) (Artifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.byID[id]
	if !ok {
		return Artifact{}, ErrNotFound
	}
	return a, nil
}

// GetByRunAndName returns a single artifact by (runID, name).
func (m *Memory) GetByRunAndName(_ context.Context, runID uuid.UUID, name string) (Artifact, error) {
	arts, err := m.GetByRunID(context.Background(), runID)
	if err != nil {
		return Artifact{}, err
	}
	for _, a := range arts {
		if a.Name == name {
			return a, nil
		}
	}
	return Artifact{}, ErrNotFound
}

// Delete removes an artifact.
func (m *Memory) Delete(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.byID[id]
	if !ok {
		return ErrNotFound
	}
	delete(m.byID, id)
	ids := m.byRunID[a.RunID]
	for i, existing := range ids {
		if existing == id {
			m.byRunID[a.RunID] = append(ids[:i], ids[i+1:]...)
			break
		}
	}
	if len(m.byRunID[a.RunID]) == 0 {
		delete(m.byRunID, a.RunID)
	}
	return nil
}

// FindExpired returns artifacts whose Expiration has passed.
func (m *Memory) FindExpired(_ context.Context, now time.Time, limit int) ([]Artifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Artifact
	for _, a := range m.byID {
		if a.ExpiresAt != nil && a.ExpiresAt.Before(now) {
			out = append(out, a)
			if len(out) >= limit {
				break
			}
		}
	}
	// Deterministic ordering for testability.
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

// MemoryPreSigner is a no-op PreSignedURLProvider for testing. It returns
// fake URLs that encode the key so assertions can verify the key is correct.
type MemoryPreSigner struct {
	Prefix string
}

// UploadURL returns a fake presigned upload URL.
func (m *MemoryPreSigner) UploadURL(_ context.Context, key string, _ string, _ int64, _ time.Duration) (string, error) {
	return m.Prefix + "/" + key, nil
}

// DownloadURL returns a fake presigned download URL.
func (m *MemoryPreSigner) DownloadURL(_ context.Context, key string, _ time.Duration) (string, error) {
	return m.Prefix + "/" + key, nil
}

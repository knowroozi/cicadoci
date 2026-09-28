package blobstore

import (
	"bytes"
	"context"
	"io"
	"sync"
)

// Memory is an in-memory BlobStore. It is safe for concurrent use and is
// primarily intended for tests. It implements both BlobStore and Reader.
//
// The in-memory store is NOT a substitute for a real cold-storage
// driver in production — it does not survive a process restart and it
// holds every blob in the heap. It exists so unit tests can exercise
// the compaction + read-through path without standing up a MinIO or a
// fake S3.
type Memory struct {
	mu    sync.RWMutex
	blobs map[string][]byte
}

// NewMemory returns an empty in-memory BlobStore.
func NewMemory() *Memory {
	return &Memory{blobs: make(map[string][]byte)}
}

// Put stores a copy of data under key.
func (m *Memory) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Copy so the caller cannot mutate the stored bytes after Put returns.
	cp := make([]byte, len(data))
	copy(cp, data)
	m.blobs[key] = cp
	return nil
}

// Get returns a copy of the bytes stored under key.
func (m *Memory) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.blobs[key]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

// Exists reports whether key is present.
func (m *Memory) Exists(_ context.Context, key string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.blobs[key]
	return ok, nil
}

// Open implements Reader.
func (m *Memory) Open(_ context.Context, key string) (io.ReadCloser, error) {
	b, err := m.Get(context.Background(), key)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Len returns the number of stored blobs. Test only.
func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.blobs)
}

// Compile-time interface checks.
var (
	_ BlobStore = (*Memory)(nil)
	_ Reader    = (*Memory)(nil)
)

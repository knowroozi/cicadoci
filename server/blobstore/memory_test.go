package blobstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestMemory_PutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	data := []byte("hello world")
	if err := m.Put(ctx, "k1", data); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := m.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("mismatch: got %q want %q", got, data)
	}
	// Mutating the original input must not affect the stored copy.
	data[0] = 'X'
	got, _ = m.Get(ctx, "k1")
	if got[0] != 'h' {
		t.Fatalf("storage not copied on Put: %q", got)
	}
	// Mutating the returned slice must not affect storage.
	got[0] = 'Y'
	got2, _ := m.Get(ctx, "k1")
	if got2[0] != 'h' {
		t.Fatalf("storage not copied on Get: %q", got2)
	}
}

func TestMemory_GetMissing(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	_, err := m.Get(ctx, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMemory_Exists(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if ok, _ := m.Exists(ctx, "x"); ok {
		t.Fatal("unexpected Exists on empty store")
	}
	_ = m.Put(ctx, "x", []byte("y"))
	if ok, err := m.Exists(ctx, "x"); err != nil || !ok {
		t.Fatalf("Exists after Put: ok=%v err=%v", ok, err)
	}
}

func TestMemory_Overwrite(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	_ = m.Put(ctx, "k", []byte("v1"))
	_ = m.Put(ctx, "k", []byte("v2"))
	got, _ := m.Get(ctx, "k")
	if string(got) != "v2" {
		t.Fatalf("overwrite failed: %q", got)
	}
	if m.Len() != 1 {
		t.Fatalf("expected 1 blob, got %d", m.Len())
	}
}

func TestMemory_OpenStreamsData(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	_ = m.Put(ctx, "k", []byte("abcabcabc"))
	rc, err := m.Open(ctx, "k")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = rc.Close() }() //nolint:errcheck
	buf, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "abcabcabc" {
		t.Fatalf("got %q", buf)
	}
}

func TestMemory_OpenMissing(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	_, err := m.Open(ctx, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMemory_Concurrent(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	// Concurrent Put + Get + Exists on distinct keys. The -race flag will
	// catch any data-race.
	for i := 0; i < 100; i++ {
		go func(i int) {
			_ = m.Put(ctx, "k", []byte("v"))
			_, _ = m.Get(ctx, "k")
			_, _ = m.Exists(ctx, "k")
		}(i)
	}
}

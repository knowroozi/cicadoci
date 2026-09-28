package secrets

import (
	"context"
	"testing"
	"time"
)

func TestGenerateKey(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(k))
	}
	k2, _ := GenerateKey()
	if k == k2 {
		t.Error("two generated keys should be different")
	}
}

func TestPutAndGet(t *testing.T) {
	key, _ := GenerateKey()
	m := NewMemory(key)
	ctx := context.Background()

	err := m.Put(ctx, "org-1", "repo-1", "API_KEY", []byte("s3cr3t"), []string{"prod"})
	if err != nil {
		t.Fatal(err)
	}

	lease, err := m.Get(ctx, "org-1", "repo-1", "API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if string(lease.Value) != "s3cr3t" {
		t.Fatalf("expected 's3cr3t', got %q", string(lease.Value))
	}
	if lease.Name != "API_KEY" {
		t.Fatalf("expected API_KEY, got %q", lease.Name)
	}
}

func TestGetNotFound(t *testing.T) {
	key, _ := GenerateKey()
	m := NewMemory(key)
	_, err := m.Get(context.Background(), "org-1", "repo-1", "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
}

func TestDelete(t *testing.T) {
	key, _ := GenerateKey()
	m := NewMemory(key)
	ctx := context.Background()

	_ = m.Put(ctx, "org-1", "repo-1", "TOKEN", []byte("abc"), nil)
	_ = m.Delete(ctx, "org-1", "repo-1", "TOKEN")
	_, err := m.Get(ctx, "org-1", "repo-1", "TOKEN")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestListNamesOnly(t *testing.T) {
	key, _ := GenerateKey()
	m := NewMemory(key)
	ctx := context.Background()

	_ = m.Put(ctx, "org-1", "repo-1", "FOO", []byte("bar-baz"), nil)
	_ = m.Put(ctx, "org-1", "repo-1", "BAZ", []byte("qux"), nil)
	_ = m.Put(ctx, "org-1", "repo-2", "OTHER", []byte("data"), nil)

	names, err := m.List(ctx, "org-1", "repo-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d: %v", len(names), names)
	}
	for _, n := range names {
		if n == "bar-baz" || n == "qux" {
			t.Fatal("List should not expose secret values, only names")
		}
	}
}

func TestLeaseExpiry(t *testing.T) {
	key, _ := GenerateKey()
	m := NewMemory(key)
	m.expiry = 50 * time.Millisecond

	_ = m.Put(context.Background(), "org-1", "repo-1", "K", []byte("v"), nil)
	lease, err := m.Get(context.Background(), "org-1", "repo-1", "K")
	if err != nil {
		t.Fatal(err)
	}

	if lease.Expired() {
		t.Fatal("lease should not be expired immediately")
	}
	time.Sleep(60 * time.Millisecond)
	if !lease.Expired() {
		t.Fatal("lease should be expired after exceeding expiry")
	}
}

func TestLeaseClose(t *testing.T) {
	key, _ := GenerateKey()
	m := NewMemory(key)

	_ = m.Put(context.Background(), "org-1", "repo-1", "K", []byte("secret-value"), nil)
	lease, _ := m.Get(context.Background(), "org-1", "repo-1", "K")
	lease.Close()
	if !lease.zeroed {
		t.Error("expected lease to be marked zeroed")
	}
	for i, b := range lease.Value {
		if b != 0 {
			t.Fatalf("expected byte %d to be 0 after Close(), got %d", i, b)
		}
	}
}

func TestTamperDetection(t *testing.T) {
	key, _ := GenerateKey()
	m := NewMemory(key)
	ctx := context.Background()

	_ = m.Put(ctx, "org-1", "repo-1", "K", []byte("data"), nil)

	m.mu.Lock()
	id := compositeKey("org-1", "repo-1", "K")
	m.store[id].Ciphertext[0] ^= 0xFF
	m.mu.Unlock()

	_, err := m.Get(ctx, "org-1", "repo-1", "K")
	if err == nil {
		t.Fatal("expected tamper detection to fail")
	}
}

func TestVerifyKey(t *testing.T) {
	key, _ := GenerateKey()
	m := NewMemory(key)
	ctx := context.Background()

	_ = m.Put(ctx, "org-1", "repo-1", "K", []byte("test"), nil)

	m.mu.Lock()
	rec := m.store[compositeKey("org-1", "repo-1", "K")]
	m.mu.Unlock()

	if err := VerifyKey(key, rec.Nonce, rec.Ciphertext); err != nil {
		t.Fatalf("expected valid key to verify, got %v", err)
	}
}

func TestConstantTimeCompare(t *testing.T) {
	if !ConstantTimeCompare([]byte("abc"), []byte("abc")) {
		t.Error("equal slices should match")
	}
	if ConstantTimeCompare([]byte("abc"), []byte("abd")) {
		t.Error("different slices should not match")
	}
}

func TestSecretsInterfaceImplemented(t *testing.T) {
	var _ Store = NewMemory(mustGenerateKey())
}

func mustGenerateKey() EncryptionKey {
	k, err := GenerateKey()
	if err != nil {
		panic(err)
	}
	return k
}

func TestEncryptionKey_Zero(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	key.Zero()
	// All bytes should be zero
	for i, b := range key {
		if b != 0 {
			t.Errorf("Zero: byte %d = %d, expected 0", i, b)
		}
	}
}

// TestListDelimitersNoConfusion verifies that orgID/repoID containing "/"
// cannot leak into an unrelated tenant's List result.
func TestListDelimitersNoConfusion(t *testing.T) {
	ctx := context.Background()
	key, _ := GenerateKey()
	defer key.Zero()

	m := NewMemory(key)

	// Store a secret under org "acme/core", repo "X", name "Y".
	if err := m.Put(ctx, "acme/core", "X", "Y", []byte("secret"), nil); err != nil {
		// "acme/core" contains "/" — that's a valid GitHub org name, but
		// our new NUL separator means the composite key is unambiguous.
		t.Logf("Put with slash in orgID returned error: %v", err)
		return
	}

	// A completely unrelated tenant: org "acme", repo "core".
	names, _ := m.List(ctx, "acme", "core")
	for _, n := range names {
		if n == "X/Y" {
			t.Fatal("delimiter confusion: unrelated org/repo pair picked up secret from acme/core/X/Y")
		}
	}
}

// TestValidateIDSepRejection verifies that NUL-byte injection is caught.
func TestValidateIDSepRejection(t *testing.T) {
	ctx := context.Background()
	key, _ := GenerateKey()
	defer key.Zero()
	m := NewMemory(key)

	if err := m.Put(ctx, "org\x00inject", "repo", "name", []byte("x"), nil); err == nil {
		t.Fatal("expected NUL byte rejection in orgID, got nil")
	}
	if err := m.Put(ctx, "org", "repo\x00inject", "name", []byte("x"), nil); err == nil {
		t.Fatal("expected NUL byte rejection in repoID, got nil")
	}
	if err := m.Put(ctx, "org", "repo", "na\x00me", []byte("x"), nil); err == nil {
		t.Fatal("expected NUL byte rejection in name, got nil")
	}
}

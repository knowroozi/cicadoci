package artifacts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMemory_CreateGetByRun(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	runID := uuid.New()
	art := Artifact{OrgID: uuid.New(), RunID: runID, Name: "binary", BlobKey: "o/binary", SizeBytes: 42}
	created, err := m.Create(ctx, art)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatal("expected non-nil ID")
	}
	arts, err := m.GetByRunID(ctx, runID)
	if err != nil {
		t.Fatalf("get by run: %v", err)
	}
	if len(arts) != 1 || arts[0].Name != "binary" {
		t.Fatalf("unexpected result: %+v", arts)
	}
}

func TestMemory_GetByRunAndName(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	runID := uuid.New()
	_, _ = m.Create(ctx, Artifact{OrgID: uuid.New(), RunID: runID, Name: "a", BlobKey: "k-a"})
	_, _ = m.Create(ctx, Artifact{OrgID: uuid.New(), RunID: runID, Name: "b", BlobKey: "k-b"})

	got, err := m.GetByRunAndName(ctx, runID, "b")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "b" {
		t.Fatalf("got %s, want b", got.Name)
	}
	_, err = m.GetByRunAndName(ctx, runID, "c")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMemory_Delete(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	runID := uuid.New()
	created, _ := m.Create(ctx, Artifact{OrgID: uuid.New(), RunID: runID, Name: "a"})
	if err := m.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := m.GetByID(ctx, created.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	arts, _ := m.GetByRunID(ctx, runID)
	if len(arts) != 0 {
		t.Fatalf("expected empty after delete, got %d", len(arts))
	}
}

func TestMemory_FindExpired(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	_, _ = m.Create(ctx, Artifact{OrgID: uuid.New(), RunID: uuid.New(), Name: "old", ExpiresAt: &past})
	_, _ = m.Create(ctx, Artifact{OrgID: uuid.New(), RunID: uuid.New(), Name: "new", ExpiresAt: &future})
	_, _ = m.Create(ctx, Artifact{OrgID: uuid.New(), RunID: uuid.New(), Name: "none"})

	got, err := m.FindExpired(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(got) != 1 || got[0].Name != "old" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestMemoryPreSigner(t *testing.T) {
	ctx := context.Background()
	ps := &MemoryPreSigner{Prefix: "https://fake.s3"}
	uploadURL, err := ps.UploadURL(ctx, "o/run/a", "application/octet-stream", 1024, time.Hour)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if uploadURL != "https://fake.s3/o/run/a" {
		t.Fatalf("got %s", uploadURL)
	}
	downloadURL, err := ps.DownloadURL(ctx, "o/run/a", time.Hour)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if downloadURL != "https://fake.s3/o/run/a" {
		t.Fatalf("got %s", downloadURL)
	}
}

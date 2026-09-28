// Package blobstore defines the cold-storage seam for log archives.
//
// Hot logs live in Postgres during a run (see server/runnersvc log
// stream); on completion they are compacted into a single blob and written
// here, the Postgres rows are pruned, and a `log_archives` row points at
// the object key. Reads fall through to the blob store transparently.
//
// BlobStore is the minimal seam for CIC-32 (M4). S3 is the first driver;
// a second driver (GCS, Azure, local FS) is expected to conform with no
// change to callers.
package blobstore

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Get when the object key does not exist.
//
// It is an exported sentinel so callers can do errors.Is(err, blobstore.ErrNotFound)
// without importing a concrete driver.
var ErrNotFound = errors.New("blobstore: object not found")

// BlobInfo describes a stored blob without its bytes. Useful for listing
// and for read-through checks before a Get.
type BlobInfo struct {
	Key   string
	Bytes int64
}

// BlobStore is the cold-storage seam.
//
// Implementations must:
//   - be safe for concurrent use.
//   - return ErrNotFound (wrapped or direct) when the key is absent.
//   - treat Put as idempotent: re-putting the same key overwrites.
//   - be safe to call from a fast path (read-through on every log page
//     load) — caching is the caller's job.
type BlobStore interface {
	// Put stores data under key. Overwrites any existing blob.
	Put(ctx context.Context, key string, data []byte) error

	// Get returns the bytes for key. Returns ErrNotFound if absent.
	Get(ctx context.Context, key string) ([]byte, error)

	// Exists reports whether key is present. Cheap; no body download.
	Exists(ctx context.Context, key string) (bool, error)
}

// Reader is the streaming variant of BlobStore.Get, for large blobs.
//
// A blob body can be many MB; materializing it into a slice on
// every read page is wasteful when the user only wants the tail.
type Reader interface {
	// Open returns an io.ReadCloser for key. The caller MUST Close.
	//
	// Returns ErrNotFound if the key is absent.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

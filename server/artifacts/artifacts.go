// Package artifacts implements the artifact storage and retrieval layer.
//
// Artifacts are per-run binary outputs (build products, test reports, etc.)
// that the runner uploads directly to object storage via presigned URLs.
// The control plane records metadata (name, size, content-type, key,
// retention) in the `artifacts` table and serves download URLs to
// authenticated consumers.
//
// Key design decisions:
//
//   - Per-run namespacing: the blob key is `{org_slug}/{run_id}/{artifact_name}`
//     so artifacts are isolated per org and per run.
//   - Size caps: a per-artifact size limit is enforced at the API layer
//     (the presigned upload URL includes an `x-amz-content-length-range`
//     for S3; the Memory driver enforces it in `Put`).
//   - Retention: each artifact carries an `expires_at` timestamp. A
//     background reaper deletes expired artifacts (blob + DB row).
//   - Security: presigned URLs are the only write path; the DB only
//     stores the blob key. Read access is via `GetObjectURL` (presigned
//     download URL) which is scoped to the requesting user's org.
package artifacts

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned when an artifact by the given (run, name) pair
// does not exist.
var ErrNotFound = errors.New("artifacts: artifact not found")

// ErrSizeExceeded is returned when an artifact exceeds the configured
// per-artifact size cap.
var ErrSizeExceeded = errors.New("artifacts: size limit exceeded")

// Artifact is a single stored artifact.
type Artifact struct {
	ID          uuid.UUID
	OrgID       uuid.UUID
	RunID       uuid.UUID
	JobID       uuid.UUID // zero if the artifact is a run-level artifact
	Name        string
	BlobKey     string // the underlying blob-store key
	SizeBytes   int64
	ContentType string
	UploadedAt  time.Time
	ExpiresAt   *time.Time
	CreatedAt   time.Time
}

// NewArtifact returns an Artifact with ID, UploadedAt, and CreatedAt
// pre-filled. The caller fills in OrgID, RunID, Name, BlobKey, etc.
func NewArtifact() Artifact {
	now := time.Now()
	return Artifact{
		ID:         uuid.New(),
		UploadedAt: now,
		CreatedAt:  now,
	}
}

// Store is the persistence seam for artifact metadata.
//
// BlobStore (from server/blobstore) is the underlying object store; Store
// records the *metadata* (name, size, key, org, run, job, expiration) in
// Postgres. The two are complementary: BlobStore holds the bytes, Store
// holds the index.
type Store interface {
	// Create records a new artifact. The BlobKey, SizeBytes, and
	// ContentType must be set.
	Create(ctx context.Context, a Artifact) (Artifact, error)

	// GetByRunID returns all artifacts for a run.
	GetByRunID(ctx context.Context, runID uuid.UUID) ([]Artifact, error)

	// GetByID returns a single artifact by its primary key.
	GetByID(ctx context.Context, id uuid.UUID) (Artifact, error)

	// GetByRunAndName returns a single artifact by (run_id, name).
	GetByRunAndName(ctx context.Context, runID uuid.UUID, name string) (Artifact, error)

	// Delete removes the DB row for an artifact. The caller is
	// responsible for also deleting the blob.
	Delete(ctx context.Context, id uuid.UUID) error

	// FindExpired returns artifacts whose Expiration has passed.
	FindExpired(ctx context.Context, now time.Time, limit int) ([]Artifact, error)
}

// PreSignedURLProvider is the seam for generating presigned blob URLs.
//
// S3 driver: wraps aws-sdk-go's S3.Client.GeneratePresignedRequest.
// Memory driver: returns a data: URL or a fake URL for testing.
//
// The S3 driver is deferred to M9 (Terraform/AMI/cloud-init), when the
// bucket + credentials are available. MemoryPreSigner is the only
// implementation in M1–M8. See docs/decisions/CIC-73-presigned-url-defer.md.
type PreSignedURLProvider interface {
	// UploadURL returns a presigned upload URL for the given key.
	// contentType is the MIME type the uploader sets in the PUT request.
	// maxBytes caps the upload size (0 = unlimited).
	UploadURL(ctx context.Context, key string, contentType string, maxBytes int64, expiry time.Duration) (url string, err error)

	// DownloadURL returns a presigned download URL for the given key.
	DownloadURL(ctx context.Context, key string, expiry time.Duration) (url string, err error)
}

// RetentionConfig defines how long artifacts live.
type RetentionConfig struct {
	// Default is the TTL for artifacts created without an explicit
	// expiration.
	DefaultDuration time.Duration
	// MaxTTL is the maximum TTL any caller may request.
	MaxTTL time.Duration
}

// DefaultRetention is the MVP default: 30-day TTL, 1-year max.
func DefaultRetention() RetentionConfig {
	return RetentionConfig{
		DefaultDuration: 30 * 24 * time.Hour,
		MaxTTL:          365 * 24 * time.Hour,
	}
}

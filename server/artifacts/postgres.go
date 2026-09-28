package artifacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Postgres is a Store backed by a SQL connection.
//
// It stores artifact metadata in the `artifacts` table (migration 0003).
// The BlobStore holds the actual bytes; this store holds the index.
type Postgres struct {
	db  *sql.DB
	org func(ctx context.Context, orgID uuid.UUID) (slug string, err error) // resolves org_id → slug for blob keying
}

// NewPostgres returns a Postgres artifact store. The orgResolver fetches
// an org's slug by ID (used to build blob keys). It must be non-nil.
func NewPostgres(db *sql.DB, orgResolver func(ctx context.Context, orgID uuid.UUID) (string, error)) *Postgres {
	if db == nil {
		panic("artifacts.NewPostgres: db must not be nil")
	}
	if orgResolver == nil {
		panic("artifacts.NewPostgres: orgResolver must not be nil")
	}
	return &Postgres{db: db, org: orgResolver}
}

const selectCols = `id, org_id, run_id, COALESCE(job_id, '00000000-0000-0000-000000000000'::uuid),
       name, blob_key, size_bytes, content_type, uploaded_at, expires_at, created_at`

// Create inserts a new artifact into the `artifacts` table.
func (p *Postgres) Create(ctx context.Context, a Artifact) (Artifact, error) {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	if a.UploadedAt.IsZero() {
		a.UploadedAt = a.CreatedAt
	}

	blobKey := a.BlobKey
	if blobKey == "" {
		slug, err := p.org(ctx, a.OrgID)
		if err != nil {
			return Artifact{}, fmt.Errorf("artifacts: resolve org slug: %w", err)
		}
		blobKey = fmt.Sprintf("%s/%s/%s", slug, a.RunID.String(), a.Name)
	}

	var jobID any
	if a.JobID != uuid.Nil {
		jobID = a.JobID
	}

	var expiresAt any
	if a.ExpiresAt != nil {
		expiresAt = a.ExpiresAt
	}

	_, err := p.db.ExecContext(ctx, `
		INSERT INTO artifacts (id, org_id, run_id, job_id, name, blob_key,
		                       size_bytes, content_type, uploaded_at, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		a.ID, a.OrgID, a.RunID, jobID, a.Name, blobKey, a.SizeBytes,
		a.ContentType, a.UploadedAt, expiresAt, a.CreatedAt,
	)
	if err != nil {
		return Artifact{}, fmt.Errorf("artifacts: create: %w", err)
	}
	a.BlobKey = blobKey
	return a, nil
}

// GetByRunID returns all artifacts for a run, ordered by created_at.
func (p *Postgres) GetByRunID(ctx context.Context, runID uuid.UUID) ([]Artifact, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT `+selectCols+` FROM artifacts WHERE run_id = $1 ORDER BY created_at`, runID)
	if err != nil {
		return nil, fmt.Errorf("artifacts: query by run: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck
	return scanRows(rows)
}

// GetByID returns a single artifact by ID.
func (p *Postgres) GetByID(ctx context.Context, id uuid.UUID) (Artifact, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT `+selectCols+` FROM artifacts WHERE id = $1`, id)
	return scanRow(row)
}

// GetByRunAndName returns an artifact by (run_id, name).
func (p *Postgres) GetByRunAndName(ctx context.Context, runID uuid.UUID, name string) (Artifact, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT `+selectCols+` FROM artifacts WHERE run_id = $1 AND name = $2`, runID, name)
	return scanRow(row)
}

// Delete removes an artifact row.
func (p *Postgres) Delete(ctx context.Context, id uuid.UUID) error {
	res, err := p.db.ExecContext(ctx, `DELETE FROM artifacts WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("artifacts: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("artifacts: delete: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// FindExpired returns artifacts past their expiration.
func (p *Postgres) FindExpired(ctx context.Context, now time.Time, limit int) ([]Artifact, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT `+selectCols+`
		FROM artifacts
		WHERE expires_at IS NOT NULL AND expires_at < $1
		ORDER BY expires_at
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("artifacts: find expired: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck
	return scanRows(rows)
}

// ── scan helpers ──────────────────────────────────────────────────

func scanRows(rows *sql.Rows) ([]Artifact, error) {
	var out []Artifact
	for rows.Next() {
		a, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: iterate rows: %w", err)
	}
	return out, nil
}

func scanRow(scanner interface {
	Scan(dest ...any) error
}) (Artifact, error) {
	var (
		a       Artifact
		jobID   string
		expires sql.NullTime
	)
	err := scanner.Scan(
		&a.ID, &a.OrgID, &a.RunID, &jobID,
		&a.Name, &a.BlobKey, &a.SizeBytes, &a.ContentType,
		&a.UploadedAt, &expires, &a.CreatedAt,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Artifact{}, ErrNotFound
	case err != nil:
		return Artifact{}, fmt.Errorf("artifacts: scan: %w", err)
	}
	if jobID != "00000000-0000-0000-0000-000000000000" {
		jid, _ := uuid.Parse(jobID)
		a.JobID = jid
	}
	if expires.Valid {
		a.ExpiresAt = &expires.Time
	}
	return a, nil
}

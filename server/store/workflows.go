package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// WorkflowInput is the write projection of a workflows row. It is the inverse of
// WorkflowRow: a caller that has parsed a workflow's YAML hands the store the
// parsed form and the store persists it, so a later StoreLoader.LoadAll can read
// it back for job creation.
//
// Source is the raw YAML, kept so the UI can show the exact bytes a run was
// produced from and so the parser's positional errors point at something that
// still exists. Definition is the JSON-encoded workflow.Workflow that
// StoreLoader decodes. The two-source representation (raw YAML + parsed JSON) is
// deliberate: the source is for humans, the parsed form is for the DAG builder.
//
// The store is the data layer and does not parse YAML; a caller builds
// Definition before calling UpsertWorkflow.
type WorkflowInput struct {
	RepoID     uuid.UUID
	Name       string
	Path       string
	Source     string
	Definition []byte // json.Marshal(workflow.Workflow)
	ContentSHA string
}

// UpsertWorkflow inserts or updates a workflow row, keyed by (repo_id, path).
//
// It is idempotent: re-registering the same path overwrites name, source,
// definition, and content_sha and bumps updated_at. The UNIQUE(repo_id, path)
// constraint is what makes the same path a single, latest-winning row, matching
// the StoreLoader's "latest version of each workflow path" read.
//
// It assumes the caller has already parsed and validated the definition, and that
// RepoID and Path are non-zero; the database enforces the repo foreign key and
// NOT NULL on path independently.
func (s *Store) UpsertWorkflow(ctx context.Context, in WorkflowInput) (uuid.UUID, error) {
	if in.RepoID == uuid.Nil || in.Path == "" {
		return uuid.Nil, fmt.Errorf("upsert workflow: repo id and path are required")
	}
	const q = `
        INSERT INTO workflows (repo_id, name, path, source, definition, content_sha)
        VALUES ($1, $2, $3, $4, $5, $6)
        ON CONFLICT (repo_id, path) DO UPDATE SET
            name         = excluded.name,
            source       = excluded.source,
            definition   = excluded.definition,
            content_sha = excluded.content_sha,
            updated_at   = now()
        RETURNING id`
	var id uuid.UUID
	// Bind in column-list order. definition is jsonb; pass the []byte directly so
	// Postgres decodes it as JSON. source and content_sha are nullable text
	// columns; empty strings store as ''. The column list and the arg list MUST
	// stay in the same order or jsonb receives the wrong value.
	err := s.pool.QueryRow(ctx, q,
		in.RepoID,
		in.Name,
		in.Path,
		in.Source,
		in.Definition,
		in.ContentSHA,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("upsert workflow %s for repo %s: %w", in.Path, in.RepoID, err)
	}
	return id, nil
}

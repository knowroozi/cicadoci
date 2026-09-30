// The read layer is the read-only half of the store, kept separate from the
// claim/queue machinery because it has a different shape and a different
// consumer. The queue (queue.Postgres) is the *writer* side of the jobs table:
// it claims, renews, and completes leases. The Reader here is the *reader* side
// everything else builds on -- the HTTP API (CIC-11), the web UI (M6), and the
// MCP server (M8) all read through it. Splitting the two keeps the hot claim
// path from accumulating list/projection queries and lets the read seam shrink
// or grow without touching the lease logic.
//
// It is an interface at the store layer by deliberate choice, exactly as
// queue.Queue is a seam for a future NATS/SQS queue: the API is written against
// Reader, so a fake Reader tests the API with no database, and a future driver
// (read replica, a different storage) can satisfy the same seam.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned by the read methods when a single-row lookup matches
// nothing. It is a sentinel (not a wrapped pgx.ErrNoRows) so callers in higher
// layers match it with errors.Is without a dependency on the storage driver.
var ErrNotFound = errors.New("store: no such row")

// Reader is the read-only surface the HTTP API and friends build on. It is
// deliberately narrow: it projects rows this project needs to *show* things,
// not the claims and leases that *drive* execution.
type Reader interface {
	// ListRuns returns runs most-recently-created first. A second return value
	// reports whether a next page exists; a zero or negative Limit returns
	// everything and reports no further page.
	ListRuns(ctx context.Context, opts ListRunsOptions) ([]Run, bool, error)
	// GetRun loads one run or ErrNotFound.
	GetRun(ctx context.Context, runID uuid.UUID) (Run, error)
	// ListRunners returns the runner fleet, newest registration first.
	ListRunners(ctx context.Context) ([]Runner, error)
	// Ping is the /readyz probe: a control plane that cannot reach its database
	// is not ready to serve traffic.
	Ping(ctx context.Context) error
}

// Run is a read-side projection of a runs row. The nullable timestamps and
// ref/sha are pointers so JSON marshals them as null rather than a zero value.
type Run struct {
	ID            uuid.UUID
	RepoID        uuid.UUID
	Number        int64
	Status        string
	Event         string
	Ref           *string
	SHA           *string
	Actor         string
	TriggerSource string
	IsFork        bool
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
}

// Runner is a read-side projection of a runners row -- enough for a fleet-status
// view. A runner that has never heartbeated has a nil LastHeartbeat.
type Runner struct {
	ID            uuid.UUID
	OrgID         uuid.UUID
	Name          string
	Labels        []string
	Executors     []string
	OS            *string
	Arch          *string
	RunnerVersion string
	Status        string
	Capacity      int32
	LastHeartbeat *time.Time
}

// ListRunsOptions filters and paginates a run listing. An empty Status selects
// every state; a negative Limit means "no cap".
type ListRunsOptions struct {
	Status string
	Limit  int
}

// runSelectColumns is the projection ListRuns and GetRun both scan. Keeping it in
// one place means the two paths and the test that exercises them cannot drift
// apart on which columns exist.
const runSelectColumns = `
    id, repo_id, number, status, event, ref, sha, actor,
    trigger_source, is_fork, created_at, started_at, finished_at`

// ListRuns returns runs most-recently-created first, optionally filtered by
// status and capped at opts.Limit. It fetches one row past the cap when capped
// so it can report hasMore to the caller.
func (s *Store) ListRuns(ctx context.Context, opts ListRunsOptions) ([]Run, bool, error) {
	q := "SELECT " + runSelectColumns + " FROM runs"
	if opts.Status != "" {
		q += " WHERE status = $1"
	}
	q += " ORDER BY created_at DESC, number DESC"

	args := make([]any, 0, 2)
	if opts.Status != "" {
		args = append(args, opts.Status)
	}

	fetch := opts.Limit
	if fetch > 0 {
		// One past the cap: an extra row means a next page exists.
		args = append(args, fetch+1)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()

	runs, more, err := collectRuns(rows, fetch)
	if err != nil {
		return nil, false, err
	}
	return runs, more, nil
}

// GetRun loads one run by id or returns ErrNotFound.
func (s *Store) GetRun(ctx context.Context, runID uuid.UUID) (Run, error) {
	var r Run
	err := s.pool.QueryRow(ctx, "SELECT "+runSelectColumns+" FROM runs WHERE id = $1", runID).
		Scan(&r.ID, &r.RepoID, &r.Number, &r.Status, &r.Event, &r.Ref, &r.SHA,
			&r.Actor, &r.TriggerSource, &r.IsFork, &r.CreatedAt, &r.StartedAt, &r.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("store: get run: %w", err)
	}
	return r, nil
}

// ListRunners returns the fleet, newest registration first.
func (s *Store) ListRunners(ctx context.Context) ([]Runner, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, name, labels, executors, os, arch,
		       runner_version, status, capacity, last_heartbeat_at
		  FROM runners
		 ORDER BY registered_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list runners: %w", err)
	}
	defer rows.Close()

	var runners []Runner
	for rows.Next() {
		var rn Runner
		if err := rows.Scan(&rn.ID, &rn.OrgID, &rn.Name, &rn.Labels, &rn.Executors,
			&rn.OS, &rn.Arch, &rn.RunnerVersion, &rn.Status, &rn.Capacity, &rn.LastHeartbeat); err != nil {
			return nil, fmt.Errorf("store: scan runner: %w", err)
		}
		runners = append(runners, rn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate runners: %w", err)
	}
	return runners, nil
}

// Ping reports whether the pool can reach Postgres. It backs /readyz.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// collectRuns drains a runs result set. When cap > 0 it stops at cap+1 rows so
// the caller learns whether a next page exists; cap <= 0 drains everything.
func collectRuns(rows pgx.Rows, cap int) ([]Run, bool, error) {
	var (
		runs []Run
		more bool
	)
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.RepoID, &r.Number, &r.Status, &r.Event, &r.Ref, &r.SHA,
			&r.Actor, &r.TriggerSource, &r.IsFork, &r.CreatedAt, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, false, fmt.Errorf("store: scan run: %w", err)
		}
		if cap > 0 && len(runs) == cap {
			// An extra row beyond the cap: the next page exists.
			more = true
			break
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: iterate runs: %w", err)
	}
	return runs, more, nil
}

// ── workflows read ───────────────────────────────────────────────────────────

// WorkflowRow is the read projection of a workflows row.
//
// Definition is the jsonb column decoded by the caller (e.g.
// StoreLoader.LoadAll in the controller package). The raw source YAML
// is also available via Source, which the UI uses to show the exact
// bytes a run was produced from.
type WorkflowRow struct {
	ID         uuid.UUID
	RepoID     uuid.UUID
	Name       string
	Path       string
	Source     string
	Definition []byte
	ContentSHA string
}

// ListWorkflowsForRepo returns all workflow rows for a repository,
// ordered by path. An empty result (no rows) is not an error.
func (s *Store) ListWorkflowsForRepo(ctx context.Context, repoID uuid.UUID) ([]WorkflowRow, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, repo_id, name, path, source, definition, content_sha
FROM workflows
WHERE repo_id = $1
ORDER BY path`, repoID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query workflows for repo: %w", err)
	}
	defer rows.Close()

	var out []WorkflowRow
	for rows.Next() {
		var w WorkflowRow
		if err := rows.Scan(&w.ID, &w.RepoID, &w.Name, &w.Path,
			&w.Source, &w.Definition, &w.ContentSHA); err != nil {
			return nil, fmt.Errorf("store: scan workflow: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate workflows: %w", err)
	}
	return out, nil
}

// var _ guards the contract: *Store must satisfy Reader. If a new method lands
// on Reader this fails to compile at the call site instead of at runtime.
var _ Reader = (*Store)(nil)

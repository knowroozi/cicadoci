// Package store adds run and job creation operations to the Store type.
//
// These are the write-side counterparts to reads.go: CreateRun and
// CreateJobSpecs open a single transaction so a run and its jobs
// land together or not at all (ADR-002). The queue NOTIFY trigger fires
// for each job whose status is set to 'queued'.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ── Trigger source constants — must match runs.trigger_source CHECK ─────────

const (
	// TriggerWeb is a run triggered from the web UI.
	TriggerWeb = "web"
	// TriggerChat is a run triggered from a Slack slash command.
	TriggerChat = "chat"
	// TriggerMCP is a run triggered from the MCP server.
	TriggerMCP = "mcp"
	// TriggerAPI is a run triggered via the REST API.
	TriggerAPI = "api"
	// TriggerForge is a run triggered by a webhook from a code host.
	TriggerForge = "forge"
	// TriggerSystem is a run triggered by the system (e.g. cron, retry).
	TriggerSystem = "system"
)

// ── Repo lookup ────────────────────────────────────────────────────────────────

// FindOrCreateRepo looks up a repo by org ID, forge, owner, and name. If it does
// not exist, it is created with the given default branch. Returns the repo UUID.
//
// The ON CONFLICT target matches the table's UNIQUE (org_id, forge, owner, name)
// composite constraint from migration 0001_init.sql.
func (s *Store) FindOrCreateRepo(
	ctx context.Context,
	orgID uuid.UUID,
	forge, owner, name, defaultBranch string,
) (uuid.UUID, error) {
	const findQ = `SELECT id FROM repos WHERE org_id = $1 AND forge = $2 AND owner = $3 AND name = $4`
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, findQ, orgID, forge, owner, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}

	const insQ = `
        INSERT INTO repos (org_id, forge, owner, name, default_branch, run_counter)
        VALUES ($1, $2, $3, $4, $5, 0)
        ON CONFLICT (org_id, forge, owner, name) DO UPDATE SET updated_at = now()
        RETURNING id`
	insErr := s.pool.QueryRow(ctx, insQ, orgID, forge, owner, name, defaultBranch).Scan(&id)
	if insErr != nil {
		return uuid.Nil, fmt.Errorf("find or create repo: %w", insErr)
	}
	return id, nil
}

// ── Run + job creation ──────────────────────────────────────────────────────────

// CreateRunOptions bundles the fields a run-creation transaction needs.
type CreateRunOptions struct {
	RepoID        uuid.UUID
	WorkflowID    *uuid.UUID
	Event         string
	Ref           string
	Sha           string
	Actor         string
	TriggerSource string
	IsFork        bool
}

// CreateRun atomically increments the repo's run counter and inserts a new
// runs row. Returns the new run ID and its 1-based number.
func (s *Store) CreateRun(ctx context.Context, opts CreateRunOptions) (runID uuid.UUID, number int64, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, 0, fmt.Errorf("begin: %w", err)
	}

	// Lock the repo row and bump the counter.
	var newNum int64
	err = tx.QueryRow(ctx, `
        UPDATE repos
        SET run_counter = run_counter + 1,
            updated_at  = now()
        WHERE id = $1
        RETURNING run_counter`, opts.RepoID,
	).Scan(&newNum)
	if err != nil {
		_ = tx.Rollback(ctx)
		return uuid.Nil, 0, fmt.Errorf("increment run_counter: %w", err)
	}

	var wfID interface{}
	if opts.WorkflowID != nil {
		wfID = *opts.WorkflowID
	}
	r := tx.QueryRow(ctx, `
        INSERT INTO runs (repo_id, workflow_id, number, status, event,
                          ref, sha, actor, trigger_source, is_fork)
        VALUES ($1, $2, $3, 'pending', $4, $5, $6, $7, $8, $9)
        RETURNING id`,
		opts.RepoID, wfID, newNum, opts.Event, opts.Ref,
		opts.Sha, opts.Actor, opts.TriggerSource, opts.IsFork,
	)
	if err = r.Scan(&runID); err != nil {
		_ = tx.Rollback(ctx)
		return uuid.Nil, 0, fmt.Errorf("insert run: %w", err)
	}

	if err = tx.Commit(ctx); err != nil {
		return uuid.Nil, 0, fmt.Errorf("commit: %w", err)
	}
	return runID, newNum, nil
}

// ── Job spec for creation ───────────────────────────────────────────────────────

// JobSpec describes one job in a run's DAG.
//
// Needs entries are names from the same spec list. A job with no Needs
// is set to 'queued' immediately (firing the NOTIFY trigger).
type JobSpec struct {
	Name         string
	Executor     string // "docker", "shell", or "cicanscript"
	RequiredTags []string
	Priority     int    // 100 default
	TimeoutSec   int    // 3600 default
	Env          []byte // JSON object; nil or empty → {}
	SecretRefs   []string
	Needs        []string
	GitCloneURL  string
	GitRef       string
	GitSha       string
	MaxAttempts  int // 3 default
}

// CreateJobSpecs atomically creates a job set and its DAG edges.
//
// All jobs start in 'pending'. Top-level jobs (empty Needs) are flipped
// to 'queued' in the same transaction, firing the NOTIFY trigger per job
// so the dispatcher picks them up promptly.
//
// Returns job IDs in the same order as specs.
func (s *Store) CreateJobSpecs(
	ctx context.Context,
	runID uuid.UUID,
	specs []JobSpec,
) ([]uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}

	ids := make([]uuid.UUID, len(specs))
	nameToIndex := make(map[string]int, len(specs))

	for i, sp := range specs {
		ids[i] = uuid.New()
		if _, dup := nameToIndex[sp.Name]; dup {
			_ = tx.Rollback(ctx)
			return nil, fmt.Errorf("duplicate job name %q", sp.Name)
		}
		nameToIndex[sp.Name] = i
	}

	// Validate Needs references.
	for _, sp := range specs {
		for _, refName := range sp.Needs {
			if _, ok := nameToIndex[refName]; !ok {
				_ = tx.Rollback(ctx)
				return nil, fmt.Errorf("job %q: needs unknown job %q", sp.Name, refName)
			}
		}
	}

	envFor := func(b []byte) []byte {
		if len(b) == 0 {
			return []byte("{}")
		}
		return b
	}

	// Insert all jobs as 'pending'.
	for i, sp := range specs {
		_, err := tx.Exec(ctx, `
        INSERT INTO jobs (id, run_id, name, executor, required_tags,
                          priority, timeout_seconds, env, secret_refs,
                          git_clone_url, git_ref, git_sha, max_attempts,
                          status, available_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
                 'pending', now())`,
			ids[i], runID, sp.Name, sp.Executor, sp.RequiredTags,
			defaultInt(sp.Priority, 100), defaultInt(sp.TimeoutSec, 3600),
			envFor(sp.Env), sp.SecretRefs,
			sp.GitCloneURL, sp.GitRef, sp.GitSha,
			defaultInt(sp.MaxAttempts, 3),
		)
		if err != nil {
			_ = tx.Rollback(ctx)
			return nil, fmt.Errorf("create job %q: %w", sp.Name, err)
		}
	}

	// Write DAG edges and flip top-level jobs to 'queued'.
	for i, sp := range specs {
		if len(sp.Needs) > 0 {
			for _, refName := range sp.Needs {
				j := nameToIndex[refName]
				_, err := tx.Exec(ctx,
					`INSERT INTO job_dependencies (job_id, depends_on_job_id) VALUES ($1, $2)`,
					ids[i], ids[j],
				)
				if err != nil {
					_ = tx.Rollback(ctx)
					return nil, fmt.Errorf("write dependency for %q: %w", sp.Name, err)
				}
			}
			continue
		}
		// Top-level: flip to 'queued' (fires NOTIFY trigger).
		_, err := tx.Exec(ctx,
			`UPDATE jobs SET status = 'queued' WHERE id = $1`,
			ids[i],
		)
		if err != nil {
			_ = tx.Rollback(ctx)
			return nil, fmt.Errorf("flip job %q to queued: %w", sp.Name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return ids, nil
}

// CompleteJob writes a terminal status to a job.
func (s *Store) CompleteJob(ctx context.Context, jobID uuid.UUID, status string, exitCode int) error {
	const q = `
        UPDATE jobs
        SET status      = $2,
            exit_code   = $3,
            finished_at = now()
        WHERE id = $1
        RETURNING id`
	var got uuid.UUID
	err := s.pool.QueryRow(ctx, q, jobID, status, exitCode).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("job %s not found", jobID)
	}
	return err
}

// ── helpers ────────────────────────────────────────────────────────────────────

func defaultInt(v, dflt int) int {
	if v <= 0 {
		return dflt
	}
	return v
}

// ── Org lookup ─────────────────────────────────────────────────────────────────

// FindOrgBySlug looks up an org by its slug. Returns uuid.Nil if not found.
func (s *Store) FindOrgBySlug(ctx context.Context, slug string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, "SELECT id FROM orgs WHERE slug = $1", slug).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, nil
		}
		return uuid.Nil, err
	}
	return id, nil
}

// UpsertOrg creates an org with the given slug/name, or returns its existing ID.
// The org's `name` field is human-readable; the `slug` field is the
// identifier used throughout the system (e.g. "acme" for "Acme Inc.").
//
// Returns the org ID (always non-nil on success).
func (s *Store) UpsertOrg(ctx context.Context, slug, name string) (uuid.UUID, error) {
	const q = `
        INSERT INTO orgs (slug, name, created_at, updated_at)
        VALUES ($1, $2, now(), now())
        ON CONFLICT (slug) DO UPDATE SET updated_at = now()
        RETURNING id`
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, q, slug, name).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("upsert org %q: %w", slug, err)
	}
	return id, nil
}

// ResolveRepo is the combined org+repo lookup used by the controller.
//
// It creates or finds the org by slug, then creates or finds the repo
// within it. Returns the repo UUID and its org UUID on success.
//
// owner is the GitHub owner (the org slug for public repos, or
// "org:user" for personal repos in the full model). For MVP,
// owner == slug, so we upsert the org with that slug.
//
// installationID is the GitHub App installation that authorized this
// event. It is stored on the repos row but is not used for auth here
// (that responsibility belongs to the webhook receiver's signature check).
func (s *Store) ResolveRepo(
	ctx context.Context,
	owner, repo, defaultBranch string,
	installationID string,
) (orgID uuid.UUID, repoID uuid.UUID, err error) {
	// Upsert org.
	orgID, err = s.UpsertOrg(ctx, owner, owner)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}

	// Upsert repo within org.  Use "github" as the default forge type.
	repoID, err = s.FindOrCreateRepo(ctx, orgID, "github", owner, repo, defaultBranch)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}

	// Update installation ID if non-empty and different.
	if installationID != "" {
		_, err := s.pool.Exec(ctx, `
            UPDATE repos
            SET installation_id = $1, updated_at = now()
            WHERE id = $2`,
			installationID, repoID,
		)
		if err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("update installation_id: %w", err)
		}
	}

	return orgID, repoID, nil
}

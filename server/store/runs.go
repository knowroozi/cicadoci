// Package store adds run and job creation operations to the Store type.
//
// These are the write-side counterparts to reads.go: CreateRun and
// CreateJobSpecs open a single transaction so a run and its jobs
// land together or not at all (ADR-002). The queue NOTIFY trigger fires
// for each job whose status is set to 'queued'.
package store

import (
	"context"
	"database/sql"
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

// FinalizeRunToSucceeded flips a pending run to 'succeeded' with
// finished_at=now().  Used by the controller when no jobs matched the
// incoming event (zero-match run finalisation, CIC-85).
//
// It is a no-op when the run is already in a non-pending state; it does
// not force a transition from 'running' or 'completed'.
func (s *Store) FinalizeRunToSucceeded(ctx context.Context, runID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
        UPDATE runs
        SET status      = 'succeeded',
            finished_at = now()
        WHERE id        = $1
        AND status      = 'pending'
    `, runID)
	if err != nil {
		return fmt.Errorf("FinalizeRunToSucceeded: %w", err)
	}
	return nil
}

// FinalizeRunToFailed flips a pending run to 'failed' with
// finished_at=now().  Used by the controller when a workflow
// matched the event but its jobs could not be created
// (e.g., DAG build failure, validation failure, job insert error).
// It is a no-op when the run is already in a non-pending state.
func (s *Store) FinalizeRunToFailed(ctx context.Context, runID uuid.UUID) (bool, error) {
	res, err := s.pool.Exec(ctx, `
        UPDATE runs
        SET status        = 'failed',
            finished_at    = now()
        WHERE id             = $1
        AND status          = 'pending'
       `, runID)
	if err != nil {
		return false, fmt.Errorf("FinalizeRunToFailed: %w", err)
	}
	return res.RowsAffected() > 0, nil
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
	Steps        []StepSpec
}

// StepSpec is a single step to create in the steps table.
type StepSpec struct {
	Name           string
	Image          string
	Command        []string
	Env            []byte // JSON object, nil or empty → {}
	If             string // CIC-118: per-step if: conditional; empty = "success"
	TimeoutSeconds int    // 0 means no timeout
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
	hold bool, // CIC-182 B2/S3: keep top-level pending when true
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
	// saFor makes a nil slice come across as an empty text[] rather than SQL NULL:
	// the jobs table marks required_tags and secret_refs NOT NULL DEFAULT '{}', so a
	// nil slice would violate the constraint and silently drop job creation. A job
	// with no tags or no secret refs is exactly the empty-array case.
	saFor := func(a []string) []string {
		if a == nil {
			return []string{}
		}
		return a
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
			ids[i], runID, sp.Name, sp.Executor, saFor(sp.RequiredTags),
			defaultInt(sp.Priority, 100), defaultInt(sp.TimeoutSec, 3600),
			envFor(sp.Env), saFor(sp.SecretRefs),
			sp.GitCloneURL, sp.GitRef, sp.GitSha,
			defaultInt(sp.MaxAttempts, 3),
		)
		if err != nil {
			_ = tx.Rollback(ctx)
			return nil, fmt.Errorf("create job %q: %w", sp.Name, err)
		}

		// Insert steps for this job (if any).
		for si, st := range sp.Steps {
			envBytes := st.Env
			if len(envBytes) == 0 {
				envBytes = []byte("{}")
			}
			cmd := st.Command
			if cmd == nil {
				cmd = []string{}
			}
			sql := `
			INSERT INTO steps
				(id, job_id, step_index, name, image, command, env,
				 timeout_seconds, network_enabled, status, if_condition)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending', $10)`
			_, err := tx.Exec(ctx, sql,
				uuid.New(), ids[i], int32(si), st.Name, st.Image,
				cmd, envBytes, int32(st.TimeoutSeconds), false, st.If)
			if err != nil {
				_ = tx.Rollback(ctx)
				return nil, fmt.Errorf("create step %d of job %q: %w", si, sp.Name, err)
			}
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
		if !hold {
			// Top-level: flip to 'queued' unless hold=true (CIC-182 B2/S3).
			_, err := tx.Exec(ctx,
				`UPDATE jobs SET status = 'queued' WHERE id = $1`,
				ids[i],
			)
			if err != nil {
				_ = tx.Rollback(ctx)
				return nil, fmt.Errorf("flip job %q to queued: %w", sp.Name, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return ids, nil
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

// ── forge check reporting (CIC-81) ───────────────────────────────────────────

// SetPreparedCheckID records the forge check ID returned by forge.Prepare on
// the run, so the run-terminal hook can complete the same check.
func (s *Store) SetPreparedCheckID(ctx context.Context, runID uuid.UUID, checkID int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE runs SET prepared_check_id = $2 WHERE id = $1`, runID, checkID)
	if err != nil {
		return fmt.Errorf("store: set prepared check id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RunCheckRef is what forge.Complete needs to address a run's check: the
// repo's owner and name, the run's commit and ref, and the check ID that
// forge.Prepare returned. CheckID is not Valid when no check was prepared.
type RunCheckRef struct {
	Owner   string
	Repo    string
	SHA     string
	Ref     string
	CheckID sql.NullInt64
}

// GetRunCheckRef returns the forge addressing data for runID, or ErrNotFound.
func (s *Store) GetRunCheckRef(ctx context.Context, runID uuid.UUID) (RunCheckRef, error) {
	var r RunCheckRef
	err := s.pool.QueryRow(ctx, `
SELECT re.owner, re.name, coalesce(ru.sha, ''), coalesce(ru.ref, ''), ru.prepared_check_id
  FROM runs ru
  JOIN repos re ON re.id = ru.repo_id
 WHERE ru.id = $1`, runID).Scan(&r.Owner, &r.Repo, &r.SHA, &r.Ref, &r.CheckID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunCheckRef{}, ErrNotFound
	}
	if err != nil {
		return RunCheckRef{}, fmt.Errorf("store: get run check ref: %w", err)
	}
	return r, nil
}

// RerunSource is the run a forge re-run request replays: the most recent run
// of a repo at a commit. CloneURL and CheckoutRef come from that run's jobs
// and are empty when it created none.
type RerunSource struct {
	RunID       uuid.UUID
	Event       string
	Ref         string
	IsFork      bool
	CloneURL    string
	CheckoutRef string
}

// FindRerunSource returns the most recent run of repoID at sha. ok is false
// when there is no such run.
//
// A re-run replays this run's event, ref and fork flag: check_suite and
// check_run are not workflow trigger types, and the payloads carry no fork
// signal, so the original run is the only trustworthy source for either.
func (s *Store) FindRerunSource(ctx context.Context, repoID uuid.UUID, sha string) (src RerunSource, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `
SELECT r.id, r.event, coalesce(r.ref, ''), r.is_fork,
       coalesce(j.git_clone_url, ''), coalesce(j.git_ref, '')
  FROM runs r
  LEFT JOIN LATERAL (
        SELECT git_clone_url, git_ref FROM jobs
         WHERE run_id = r.id
         ORDER BY created_at
         LIMIT 1) j ON true
 WHERE r.repo_id = $1 AND r.sha = $2
 ORDER BY r.created_at DESC, r.number DESC
 LIMIT 1`, repoID, sha).Scan(&src.RunID, &src.Event, &src.Ref, &src.IsFork, &src.CloneURL, &src.CheckoutRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return RerunSource{}, false, nil
	}
	if err != nil {
		return RerunSource{}, false, fmt.Errorf("store: find rerun source: %w", err)
	}
	return src, true, nil
}

// CountRunJobs returns how many jobs runID has. A run with none never reaches
// the run-terminal hook, so the controller finishes it at once: completes its
// check (CIC-81) and releases its backpressure slot (CIC-106).
func (s *Store) CountRunJobs(ctx context.Context, runID uuid.UUID) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE run_id = $1`, runID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count run jobs: %w", err)
	}
	return n, nil
}

// CancelRunJobs cancels every non-terminal job of a run and, when the run
// has no active (queued/claimed/running) jobs, finalizes the run to
// 'cancelled' and fires the terminal hook.
//
// The operation is a single transaction. A FOR UPDATE lock on the runs
// row (R1) ensures no concurrent advanceRun can interfere: a job that
// finishes between the read and write is caught by the tx-level lock.
//
// B1: a gate-held run has all jobs 'pending' (not in the cancel scanner's
// path). The tx UPDATEs them to 'cancelled' and finalizes the run.
//
// Active (queued/claimed/running) jobs are terminalized by the
// dispatcher's cancel scanner and the runner's cancel path, which call
// advanceRun. CancelRunJobs does NOT finalize the run when active jobs
// remain; the cancel scanner + advanceRun finalize and fire the hook.
//
// B2: a per-job cancel failure is joined; the loop continues so the other
// active jobs still get the doorbell; the handler audits when touched.
//
// CIC-208 (follow-up, not implemented here): the open approval gate
// record (timer + map entry) is NOT closed by this method.
//
// fireTerminal is called after the tx commit ONLY when THIS call
// finalized the run (the gated/pending-only case). advanceRun fires it
// for the active-job case.
//
// Returns (touched, err).
//   - touched=true: at least one job was cancelled or the run was
//     finalized. The handler must audit when touched=true, even if
//     err != nil.
//   - touched=false: no non-terminal jobs existed. No audit row.
//   - err: nil on full success; errors.Join on partial failure; raw
//     error otherwise.
func (s *Store) CancelRunJobs(
	ctx context.Context,
	runID uuid.UUID,
	reason string,
	cancelJob func(ctx context.Context, jobID uuid.UUID, reason string) error,
	fireTerminal func(ctx context.Context, runID uuid.UUID),
) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: cancel-run begin %s: %w", runID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// R1: lock the run row so a concurrent advanceRun (cancel scanner or
	// runner path) blocks until this tx commits. Without this lock,
	// a job could finish in the window between reading ActiveJobIDs
	// outside the tx and the tx commit, promoting a dependent that
	// escapes the cancel or missing the run-finalize.
	var runStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&runStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // run does not exist; no-op
	}
	if err != nil {
		return false, fmt.Errorf("store: cancel-run lock %s: %w", runID, err)
	}
	// Already-finalised run: no-op, no hook fires, no audit row.
	if runStatus != "pending" && runStatus != "running" {
		return false, nil
	}

	// Cancel top-level pending jobs (gated or dependent-only).
	res, err := tx.Exec(ctx, `
        UPDATE jobs SET status = 'cancelled', cancel_reason = $2, finished_at = now()
          WHERE run_id = $1
        AND status = 'pending'
        AND NOT EXISTS (SELECT 1 FROM job_dependencies WHERE job_id = jobs.id)`,
		runID, reason)
	if err != nil {
		return false, fmt.Errorf("store: cancel-run top-level pending %s: %w", runID, err)
	}
	touched := res.RowsAffected() > 0

	// Cancel dependent pending jobs as well.
	res, err = tx.Exec(ctx, `
        UPDATE jobs SET status = 'cancelled', cancel_reason = $2, finished_at = now()
          WHERE run_id = $1 AND status = 'pending'`, runID, reason)
	if err != nil {
		return touched, fmt.Errorf("store: cancel-run dependent pending %s: %w", runID, err)
	}
	if res.RowsAffected() > 0 {
		touched = true
	}

	// Read the active job IDs INSIDE the tx so newly-promoted jobs that
	// were pending before we started the tx are caught here too.
	rows, err := tx.Query(ctx, `
        SELECT id FROM jobs
         WHERE run_id = $1
           AND status IN ('queued', 'claimed', 'running')`, runID)
	if err != nil {
		return touched, fmt.Errorf("store: cancel-run active check %s: %w", runID, err)
	}
	activeIDs := make([]uuid.UUID, 0)
	for rows.Next() {
		var jid uuid.UUID
		if err := rows.Scan(&jid); err != nil {
			rows.Close()
			return touched, fmt.Errorf("store: cancel-run active scan %s: %w", runID, err)
		}
		activeIDs = append(activeIDs, jid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return touched, fmt.Errorf("store: cancel-run active rows err %s: %w", runID, err)
	}

	// Finalize the run to 'cancelled' ONLY when no active jobs remain.
	// With active jobs present, the cancel scanner + advanceRun will finalize
	// and fire the terminal hook. No finalize here => no double-finalize.
	fire := len(activeIDs) == 0
	if fire {
		res, err = tx.Exec(ctx, `
        UPDATE runs
        SET status     = 'cancelled',
            finished_at = now()
        WHERE id       = $1
        AND status   IN ('pending', 'running')`, runID)
		if err != nil {
			return touched, fmt.Errorf("store: cancel-run finalize %s: %w", runID, err)
		}
		fire = res.RowsAffected() > 0
		if fire {
			touched = true
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return touched, fmt.Errorf("store: cancel-run commit %s: %w", runID, err)
	}

	// Fire the terminal hook after the tx commit — matches the queue's
	// pattern (advanceRun commits, then q.fire runs outside the tx).
	// Fire only when THIS call finalized the run.
	if fire && fireTerminal != nil {
		fireTerminal(ctx, runID)
	}

	// Ring the cancel doorbell for active jobs.
	// B2: continue on per-job error; errors.Join; do not abort.
	var joinErr error
	for _, jid := range activeIDs {
		if err := cancelJob(ctx, jid, reason); err != nil {
			joinErr = errors.Join(joinErr, err)
			continue
		}
		touched = true
	}
	return touched, joinErr
}

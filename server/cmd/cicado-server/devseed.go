package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/auth"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/store"
)

// DevRegistrationToken is the fixed registration token the development stack
// enrolls its runner with.
//
// It is a constant in source, in a repository, on purpose: it exists so that
// `make dev` works with no manual bootstrap step. It is not a secret and is not
// treated as one. Seeding is refused unless explicitly enabled, and a server
// started this way says so loudly in its logs.
const DevRegistrationToken = "cicado_reg_Y2ljYWRvLWRldmVsb3BtZW50LXRva2VuLURPLU5PVCE"

// devSeed makes a fresh development database usable: one org, one registration
// token, and a demo job that prints hello.
//
// It is idempotent — `make dev` restarts the stack constantly — so it inserts
// only what is missing, and queues a fresh demo job each time so there is
// always something to watch run.
func devSeed(ctx context.Context, st *store.Store, q *queue.Postgres, log *slog.Logger) error {
	log.Warn("DEVELOPMENT SEEDING ENABLED",
		"detail", "a well-known registration token from the public repository is being installed; "+
			"never run with -dev-seed against anything you care about")

	pool := st.Pool()

	var orgID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO orgs (slug, name) VALUES ('dev', 'Development')
		ON CONFLICT (slug) DO UPDATE SET name = excluded.name
		RETURNING id`).Scan(&orgID); err != nil {
		return fmt.Errorf("seed org: %w", err)
	}

	if err := auth.Validate(auth.KindRegistration, DevRegistrationToken); err != nil {
		return fmt.Errorf("the built-in development token is malformed: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tokens (kind, token_hash, org_id, claims, max_uses)
		VALUES ('registration', $1, $2, '{"labels": ["linux", "shell", "dev"]}'::jsonb, 1000)
		ON CONFLICT (token_hash) DO NOTHING`,
		auth.Hash(DevRegistrationToken), orgID); err != nil {
		return fmt.Errorf("seed registration token: %w", err)
	}

	var repoID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO repos (org_id, owner, name, default_branch)
		VALUES ($1, 'cicadoci', 'core', 'master')
		ON CONFLICT (org_id, forge, owner, name) DO UPDATE SET updated_at = now()
		RETURNING id`, orgID).Scan(&repoID); err != nil {
		return fmt.Errorf("seed repo: %w", err)
	}

	jobID, err := seedDemoJob(ctx, st, q, repoID)
	if err != nil {
		return err
	}

	log.Info("development stack seeded",
		"org_id", orgID,
		"registration_token_hash", hex.EncodeToString(auth.Hash(DevRegistrationToken)),
		"demo_job_id", jobID)
	return nil
}

// seedDemoJob queues the hardcoded `echo hello` job. This is the M0 exit
// criterion made runnable by hand: bring up the stack and watch it execute.
func seedDemoJob(ctx context.Context, st *store.Store, q *queue.Postgres, repoID uuid.UUID) (uuid.UUID, error) {
	pool := st.Pool()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("seed demo job: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Run numbers are per repo and gapless; take the row lock to allocate one.
	var runNumber int64
	if err := tx.QueryRow(ctx, `
		UPDATE repos SET run_counter = run_counter + 1
		 WHERE id = $1 RETURNING run_counter`, repoID).Scan(&runNumber); err != nil {
		return uuid.Nil, fmt.Errorf("allocate run number: %w", err)
	}

	var runID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO runs (repo_id, number, status, event, ref, sha, actor, trigger_source)
		VALUES ($1, $2, 'running', 'manual', 'refs/heads/master', 'devseed', 'dev-seed', 'system')
		RETURNING id`, repoID, runNumber).Scan(&runID); err != nil {
		return uuid.Nil, fmt.Errorf("create demo run: %w", err)
	}

	// Created 'pending', with steps attached, and only queued at the end. A job
	// must not become claimable before it is fully constructed: the doorbell
	// wakes a dispatcher within milliseconds of the commit.
	var jobID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO jobs (run_id, name, status, executor, required_tags, env, timeout_seconds)
		VALUES ($1, 'hello', 'pending', 'shell', ARRAY['shell'], '{"CICADO_DEMO":"1"}'::jsonb, 120)
		RETURNING id`, runID).Scan(&jobID); err != nil {
		return uuid.Nil, fmt.Errorf("create demo job: %w", err)
	}

	steps := []struct {
		name    string
		command []string
	}{
		{"greet", []string{"echo", "hello from CICaDo"}},
		{"describe", []string{"sh", "-c", "echo running as $(whoami) in $(pwd)"}},
	}
	for i, step := range steps {
		if _, err := tx.Exec(ctx, `
			INSERT INTO steps (job_id, step_index, name, command, env, timeout_seconds)
			VALUES ($1, $2, $3, $4, '{}'::jsonb, 60)`,
			jobID, i, step.name, step.command); err != nil {
			return uuid.Nil, fmt.Errorf("create demo step: %w", err)
		}
	}

	// Enqueue in this same transaction that created the run and its job, rather than
	// afterwards as a separate statement on the pool: "this run exists" and "its top-
	// level job is queued" must commit as one atomic fact or not at all. A crash in
	// between two steps would leave stranded work no dispatcher ever wakes up for,
	// which is exactly the gap CIC-12 asks us to close here -- real run creation later
	// will use this same call, devseed just exercises it now so that path has a tested
	// home before then. The AFTER UPDATE trigger still rings the LISTEN/NOTIFY doorbell
	// only at COMMIT, exactly as before.
	if err := q.EnqueueInTx(ctx, tx, jobID); err != nil && !errors.Is(err, context.Canceled) {
		return uuid.Nil, fmt.Errorf("queue demo job: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit demo job: %w", err)
	}

	// The commit above is what rings the LISTEN/NOTIFY doorbell for anyone waiting:
	// a dispatcher on its own connection wakes and claims without polling.
	return jobID, nil
}

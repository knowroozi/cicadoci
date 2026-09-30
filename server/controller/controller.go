// Package controller is the first real (non-NoOp) implementation of
// webhook.Dispatcher.
//
// It turns decoded GitHub webhook events into run + job creation in the
// store, queue-enqueue via the NOTIFY trigger, and status reports to
// the configured forge.  This is CIC-71 sub-blockers 1–3.
//
// CIC-71b replaced the SHA-1 pseudo-UUID org resolution with
// store.ResolveRepo, upserting the org and repo rows on first contact.
// The installationID parameter is passed through to the store but not
// yet used for auth (that's CIC-38/39/40 — GitHub App installation tokens).
//
// CIC-71c adds workflow loading and job DAG creation (LoadAndCreateJobs).
// The WorkflowLoader interface decouples the controller from the storage
// mechanism for workflow YAML; a nil loader is a silent no-op.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/backpressure"
	"cicadoci.com/cicado/server/forge"
	"cicadoci.com/cicado/server/policy"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/webhook"
)

// Config tunes the controller.
type Config struct {
	// Backpressure, when non-nil, gates run creation on per-repo
	// concurrency limits.
	Backpressure *backpressure.Tracker
	// DefaultBranch is used when payload data is incomplete.
	DefaultBranch string
	// WorkflowLoader loads workflow definitions for a repo.
	//
	// When nil (DefaultConfig), LoadAndCreateJobs is a silent no-op:
	// runs are created but no jobs are enqueued.  This is how
	// CIC-71a/CIC-71b remain backwards-compatible.
	WorkflowLoader WorkflowLoader
	// ForkPolicy enforces the fork-PR security policy on workflow jobs.
	// When prFork is true in LoadAndCreateJobs, the policy is applied to
	// each matching workflow: secret references are scrubbed, deploy
	// identities are cleared, and ForkSafeTag is added to each job's
	// RunsOn.  A nil ForkPolicy means no fork restrictions.
	ForkPolicy *policy.ForkPolicy
}

// DefaultConfig returns a Config with sensible development values.
//
// WorkflowLoader is nil by default; production servers wire it via
// cicao-server main.go.
func DefaultConfig() Config {
	return Config{
		Backpressure:  backpressure.NewTracker(backpressure.DefaultLimits()),
		DefaultBranch: "main",
		ForkPolicy:    policy.DefaultForkPolicy(),
		// WorkflowLoader: nil
	}
}

// ── Controller ────────────────────────────────────────────────────────────────

// Controller implements webhook.Dispatcher.
//
// It creates a run in the store when a qualifying webhook event
// arrives, reports it to the forge, and enqueues its top-level jobs
// when a WorkflowLoader is configured.
type Controller struct {
	store *store.Store
	forge forge.Forge
	cfg   Config
	log   *slog.Logger
}

// New builds a Controller.
//
// Pass log = nil to use slog.Default().
func New(st *store.Store, f forge.Forge, cfg Config, log *slog.Logger) *Controller {
	if log == nil {
		log = slog.Default()
	}
	return &Controller{store: st, forge: f, cfg: cfg, log: log}
}

// ── webhook.Dispatcher ────────────────────────────────────────────────────────

// HandlePush handles GitHub push events for branch refs only.
//
// Tag pushes, refs/pull/*, and unmanaged repos are silently dropped.
func (c *Controller) HandlePush(payload webhook.PushPayload) error {
	ref := payload.Ref
	if !strings.HasPrefix(ref, "refs/heads/") {
		return nil // skip non-branch refs
	}
	branch := ref[len("refs/heads/"):]

	// Resolve the base repo from the event's Repository.FullName.
	// For push events, Repository is always the target repo.
	owner, repoName, ok := splitRepo(payload.Repository.FullName)
	if !ok {
		return fmt.Errorf("controller push: invalid repo full_name %q", payload.Repository.FullName)
	}

	ctx := context.Background()
	orgID, repoID, err := c.resolveRepo(ctx, owner, repoName)
	if err != nil {
		c.log.Warn("controller push: store lookup failed, skipping",
			"repo", payload.Repository.FullName, "err", err)
		return nil
	}

	if c.cfg.Backpressure != nil {
		ok, err := c.cfg.Backpressure.Acquire(ctx, repoID.String(), orgID.String())
		if err != nil || !ok {
			c.log.Debug("backpressure: skipping push",
				"repo", payload.Repository.FullName, "ok", ok, "err", err)
			return nil
		}
		defer c.cfg.Backpressure.Release(repoID.String(), orgID.String())
	}

	sha := payload.HeadCommit.ID
	if sha == "" {
		sha = payload.After
	}

	runID, number, err := c.store.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "push",
		Ref:           branch,
		Sha:           sha,
		Actor:         payload.Pusher.Email,
		TriggerSource: store.TriggerForge,
		IsFork:        false,
	})
	if err != nil {
		return fmt.Errorf("controller push: create run: %w", err)
	}
	c.log.Info("run created", "run", runID, "number", number,
		"event", "push", "repo", payload.Repository.FullName)

	// Create job DAG from matching workflows.
	if err := c.LoadAndCreateJobs(ctx, repoID, runID, "push", branch, sha, "", false); err != nil {
		c.log.Warn("job creation failed for push run", "run", runID, "err", err)
		// Job creation failure is non-fatal: the run was created and its
		// status can be reported separately.
	}

	// Report the new run to the forge.
	report(ctx, c.forge, owner, repoName, sha, runID, branch, c.log)
	return nil
}

// HandlePullRequest handles GitHub pull_request "opened" and "synchronize"
// sub-events.
//
// The run is created against the BASE repo (payload.Repository), not the
// contributor's fork. Head.Repo/Head.SHA/Head.Ref are used only as the
// actual clone target for the job.
func (c *Controller) HandlePullRequest(payload webhook.PullRequestPayload) error {
	switch payload.Action {
	case "opened", "synchronize":
		// proceed
	default:
		return nil
	}

	// Resolve against the base repo (payload.Repository), not the fork.
	// This prevents any GitHub user from auto-provisioning an org row
	// by opening a fork PR.
	owner, repoName, ok := splitRepo(payload.Repository.FullName)
	if !ok {
		return fmt.Errorf("controller pr: invalid base repo %q", payload.Repository.FullName)
	}

	ctx := context.Background()
	orgID, repoID, err := c.resolveRepo(ctx, owner, repoName)
	if err != nil {
		c.log.Warn("controller pr: store lookup failed, skipping",
			"repo", payload.Repository.FullName, "err", err)
		return nil
	}

	if c.cfg.Backpressure != nil {
		ok, err := c.cfg.Backpressure.Acquire(ctx, repoID.String(), orgID.String())
		if err != nil || !ok {
			c.log.Debug("backpressure: skipping pr",
				"repo", payload.Repository.FullName, "ok", ok, "err", err)
			return nil
		}
		defer c.cfg.Backpressure.Release(repoID.String(), orgID.String())
	}

	// Base.Ref is the target branch (what the PR merges into);
	// it is used for trigger matching and run correlation.
	branch := payload.Base.Ref
	prNumber := strconv.Itoa(payload.Number) // used as actor
	runID, number, err := c.store.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "pull_request",
		Ref:           branch,
		Sha:           payload.Head.SHA,
		Actor:         prNumber,
		TriggerSource: store.TriggerForge,
		IsFork:        payload.Fork,
	})
	if err != nil {
		return fmt.Errorf("controller pr: create run: %w", err)
	}
	c.log.Info("run created", "run", runID, "number", number,
		"event", "pull_request", "pr", payload.Number, "repo", payload.Repository.FullName)

	// Create job DAG from matching workflows.
	if err := c.LoadAndCreateJobs(ctx, repoID, runID, "pull_request", branch, payload.Head.SHA, payload.Action, payload.Fork); err != nil {
		c.log.Warn("job creation failed for PR run", "run", runID, "err", err)
	}

	report(ctx, c.forge, owner, repoName, payload.Head.SHA, runID, branch, c.log)
	return nil
}

// HandleCheckSuite handles check_suite "requested" events.
//
// TODO(CIC-71d): wire re-check of an existing run.
func (c *Controller) HandleCheckSuite(payload webhook.CheckSuitePayload) error {
	if payload.Action != "requested" {
		return nil
	}
	c.log.Info("check_suite requested; no-op until job resolution lands",
		"check_suite", payload.CheckSuite.ID)
	return nil
}

// HandleCheckRun handles check_run "rerequested" events.
//
// TODO(CIC-71d): wire re-check of an existing run.
func (c *Controller) HandleCheckRun(payload webhook.CheckRunPayload) error {
	if payload.Action != "rerequested" {
		return nil
	}
	c.log.Info("check_run rerequested; no-op until job resolution lands",
		"check_run", payload.CheckRun.ID)
	return nil
}

// ── helpers ────────────────────────────────────────────────────────────────────

// resolveRepo resolves a repo's full name to its org ID and repo ID
// via store.ResolveRepo. It upserts both the org (by slug) and the
// repo atomically on first contact.
//
// orgID is used for per-org backpressure tracking.
// repoID is used for CreateRunOptions.RepoID and per-repo backpressure.
//
// NOTE: this is optimistic — it creates the org row on first contact
// with no allowlist check. CIC-71d should add a registered-orgs gate
// or the operator must ensure their webhook receiver only handles
// events from known installation tokens.
func (c *Controller) resolveRepo(ctx context.Context, owner, repoName string) (
	orgID uuid.UUID,
	repoID uuid.UUID,
	err error,
) {
	// installationID is empty for now; it will be populated when the
	// webhook receiver provides the App installation ID (CIC-38/39/40).
	return c.store.ResolveRepo(ctx, owner, repoName, c.cfg.DefaultBranch, "")
}

func splitRepo(fullName string) (owner, repo string, ok bool) {
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func report(
	ctx context.Context,
	f forge.Forge,
	owner, repo, sha string,
	runID uuid.UUID,
	branch string,
	log *slog.Logger,
) {
	t := time.Now()
	if _, err := f.Prepare(ctx, forge.RunRef{
		Org:     owner,
		Repo:    repo,
		Sha:     sha,
		RunID:   runID.String(),
		Branch:  branch,
		Started: &t,
	}); err != nil {
		log.Warn("forge.Prepare failed", "run", runID, "err", err)
	}
}

// ── compile-time check ─────────────────────────────────────────────────────────

var _ webhook.Dispatcher = (*Controller)(nil)

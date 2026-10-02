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
	"strings"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/approvalgates"
	"cicadoci.com/cicado/server/audit"
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
	// A nil ForkPolicy means no restrictions.
	ForkPolicy *policy.ForkPolicy

	// Audit, when non-nil, records an audit entry when a run is
	// created.  nil is a no-op: runs are still created, jobs still
	// dispatch, but no audit trail is generated.  DefaultConfig
	// provides a MemoryLog for dev servers.
	Audit audit.Log

	// ApprovalGates, when non-nil, opens a deployment-approval
	// gate for runs that opt in via a policy.  The onResolved callback
	// (wired by the server) advances the run.  nil is a no-op.
	ApprovalGates *approvalgates.Manager

	// AllowedApprovers is the allow-list of user IDs who may resolve
	// an approval gate.  Passed to approvalgates.Gate.Allowed at gate
	// creation.  An empty slice means no one is authorized — the chat
	// buttons are inert (a future ticket should map GitHub roles to
	// CICaDo user IDs).
	AllowedApprovers []string
	// Producer fetches and upserts workflow YAMLs from a code host.
	//
	// When non-nil, HandlePush (on the default branch) calls
	// Producer.Produce before LoadAndCreateJobs so the loader sees fresh
	// workflows.  PR events must NOT call the producer.
	// When nil, the controller uses whatever is already in the store.
	Producer WorkflowProducer
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
		// Audit is nil by default; wire a real log via WithAudit().
	}
}

// WithAudit sets the audit log for run-creation events.
func (c Config) WithAudit(auditLog audit.Log) Config {
	c.Audit = auditLog
	return c
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
		c.log.Warn("controller push: store lookup failed",
			"repo", payload.Repository.FullName, "err", err)
		return fmt.Errorf("controller push: resolve repo: %w", err)
	}

	if c.cfg.Backpressure != nil {
		ok, err := c.cfg.Backpressure.Acquire(ctx, repoID.String(), orgID.String())
		if err != nil || !ok {
			c.log.Debug("backpressure: skipping push",
				"repo", payload.Repository.FullName, "ok", ok, "err", err)
			return fmt.Errorf("controller push: backpressure")
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

	// CIC-84: produce fresh workflows from the code host before loading.
	// SECURITY: only the default branch upserts into the shared repo-wide
	// workflow row. A feature-branch push that carries its own .github/
	// workflows/ must not overwrite the stored definition because a
	// on: push: branches: [main] workflow fetched from a feature branch
	// could poison the next main push and bypass branch protection.
	// SECURITY: only the default branch upserts into the shared
	// repo-wide workflow row.  A missing payload.Repository.DefaultBranch
	// (GitHub may omit it) falls back to c.cfg.DefaultBranch.
	defaultBranch := payload.Repository.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = c.cfg.DefaultBranch
	}
	if c.cfg.Producer != nil && branch == defaultBranch {
		if err := c.cfg.Producer.Produce(ctx, owner, repoName, sha, repoID); err != nil {
			c.log.Warn("producer: upsert failed; using existing store workflows",
				"repo", owner+"/"+repoName, "sha", sha, "err", err)
		}
	}

	// Create job DAG from matching workflows.
	pushCloneURL := "https://github.com/" + payload.Repository.FullName + ".git"
	if err := c.LoadAndCreateJobs(ctx, repoID, runID, "push", branch, sha,
		"" /* prAction */, false, /* isFork */
		pushCloneURL, "refs/heads/"+branch); err != nil {
		c.log.Warn("job creation failed for push run", "run", runID, "err", err)
		// Job creation failure is non-fatal: the run was created and its
		// status can be reported separately.
	}

	// Report the new run to the forge.
	report(ctx, c.forge, owner, repoName, sha, runID, branch, c.log)

	// Audit the run creation.  Non-fatal: audit failure shouldn't
	// prevent the run from executing.
	c.emitAudit(ctx, runID, "push", payload.Pusher.Email, owner, repoName, branch)
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
		c.log.Warn("controller pr: store lookup failed",
			"repo", payload.Repository.FullName, "err", err)
		return fmt.Errorf("controller pr: resolve repo: %w", err)
	}

	if c.cfg.Backpressure != nil {
		ok, err := c.cfg.Backpressure.Acquire(ctx, repoID.String(), orgID.String())
		if err != nil || !ok {
			c.log.Debug("backpressure: skipping pr",
				"repo", payload.Repository.FullName, "ok", ok, "err", err)
			return fmt.Errorf("controller pr: backpressure")
		}
		defer c.cfg.Backpressure.Release(repoID.String(), orgID.String())
	}

	// Base.Ref is the target branch (what the PR merges into);
	// it is used for trigger matching and run correlation.
	// isFork delegates to PullRequestPayload.IsFork() — fail-closed.
	isFork := payload.IsFork()
	branch := payload.PullRequest.Base.Ref
	// Audit actor: prefer the PR author's GitHub login; fall back to the
	// PR number when sender.Login is empty (e.g. bot token).  sender is
	// always present in real payloads; the fallback is for API-token
	// integrations that may have no user account.
	actor := payload.Sender.Login
	if actor == "" {
		actor = fmt.Sprintf("pr-%d", payload.PullRequest.Number)
	}
	runID, number, err := c.store.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "pull_request",
		Ref:           branch,
		Sha:           payload.PullRequest.Head.SHA,
		Actor:         actor,
		TriggerSource: store.TriggerForge,
		IsFork:        isFork,
	})
	if err != nil {
		return fmt.Errorf("controller pr: create run: %w", err)
	}
	c.log.Info("run created", "run", runID, "number", number,
		"event", "pull_request", "pr", payload.PullRequest.Number, "repo", payload.Repository.FullName)

	// Create job DAG from matching workflows.
	// SECURITY (CIC-84): PR events must NOT trigger the workflow producer.
	// The producer upserts into the repo's shared (repo_id, path) row,
	// so a same-repo PR to main could poison subsequent push runs.
	//
	// refs/pull/<N>/head only exists on the base repo, so use the base
	// repo's clone URL (payload.Repository.CloneURL, falling back to
	// github.com/<org>/<repo>.git when empty).  Trigger matching on
	// 'branch' (base ref) is separate and handled by LoadAndCreateJobs
	// internally.
	baseCloneURL := payload.Repository.CloneURL
	if baseCloneURL == "" {
		baseCloneURL = "https://github.com/" + payload.Repository.FullName + ".git"
	}
	checkoutRef := fmt.Sprintf("refs/pull/%d/head", payload.PullRequest.Number)
	if err := c.LoadAndCreateJobs(ctx, repoID, runID, "pull_request", branch, payload.PullRequest.Head.SHA, payload.Action, isFork,
		baseCloneURL, checkoutRef); err != nil {
		c.log.Warn("job creation failed for PR run", "run", runID, "err", err)
	}

	report(ctx, c.forge, owner, repoName, payload.PullRequest.Head.SHA, runID, branch, c.log)

	// Audit the run creation with the PR author's login as the actor.
	c.emitAudit(ctx, runID, "pull_request", actor, owner, repoName, branch)

	// Open a deployment-approval gate if the run requires approval.
	c.maybeOpenGate(ctx, runID, owner, repoName, payload)
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

// ── approval gates ────────────────────────────────────────────────────────────

// maybeOpenGate opens a deployment-approval gate for a pull-request run if
// the controller is configured with an approvalgates.Manager.
//
// The gate is opened non-fatally: if the provider does not support
// approvals (nil ChatProvider) or if the gate creation fails, a Warn is logged
// and the run proceeds unblocked.  This is the MVP behaviour — a policy layer
// that determines which workflows require approval is a future ticket.
func (c *Controller) maybeOpenGate(
	ctx context.Context,
	runID uuid.UUID,
	owner, repo string,
	payload webhook.PullRequestPayload,
) {
	mgr := c.cfg.ApprovalGates
	if mgr == nil {
		return
	}
	// MVP: open a gate for every PR run as a placeholder.
	// The manager itself decides whether to block based on policy.
	// A Gate must carry the allow-list of user IDs that may resolve it
	// (Gate.Allowed / authorized()); an empty list = deny by default.  Warn
	// so an operator can tell when a gate is open but inert.
	if len(c.cfg.AllowedApprovers) == 0 {
		c.log.Warn("approvalgates: gate opened with empty allow-list; wire Config.AllowedApprovers so a user can resolve it",
			"run", runID)
	}
	g := &approvalgates.Gate{
		RunID:   runID.String(),
		Channel: fmt.Sprintf("%d", payload.PullRequest.Number),
		Allow:   "approve",
		Reason:  fmt.Sprintf("PR #%d on %s/%s", payload.PullRequest.Number, owner, repo),
		Deny:    "deny",
		Allowed: c.cfg.AllowedApprovers,
	}
	if err := mgr.Open(ctx, g); err != nil {
		c.log.Warn("approvalgates: failed to open gate",
			"run", runID, "err", err)
	}
}

// ── compile-time check ─────────────────────────────────────────────────────────

var _ webhook.Dispatcher = (*Controller)(nil)

// ── audit ─────────────────────────────────────────────────────────────────────

// emitAudit records a "create_run" event in the audit log.
//
// Failures are logged but not propagated: the run was already
// created and the job DAG is building; audit failure must not
// block the run's execution.
func (c *Controller) emitAudit(
	ctx context.Context,
	runID uuid.UUID,
	eventType, actor, owner, repo, branch string,
) {
	if c.cfg.Audit == nil {
		return
	}
	_, err := c.cfg.Audit.Append(ctx, audit.Entry{
		Action: audit.ActionCreateRun,
		Actor:  actor,
		Target: fmt.Sprintf("run/%s", runID.String()),
		Reason: fmt.Sprintf("%s on %s/%s branch %q", eventType, owner, repo, branch),
	})
	if err != nil {
		c.log.Warn("audit append failed", "run", runID, "action", "create_run", "err", err)
	}
}

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
// yet used for auth (CIC-81 wires the GitHub forge; per-call installation
// token minting is CIC-146).
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
		c.releaseSlot(repoID, orgID)
		return fmt.Errorf("controller push: create run: %w", err)
	}
	c.holdSlot(runID, repoID, orgID)
	c.log.Info("run created", "run", runID, "number", number,
		"event", "push", "repo", payload.Repository.FullName)

	// Prepare the forge check before any job exists, so a run that finishes
	// fast still finds its check ID when RunCompleted fires.
	c.report(ctx, owner, repoName, sha, runID, branch)

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
	loadErr := c.LoadAndCreateJobs(ctx, repoID, runID, "push", branch, sha,
		"" /* prAction */, false, /* isFork */
		pushCloneURL, "refs/heads/"+branch)
	if loadErr != nil {
		c.log.Warn("job creation failed for push run", "run", runID, "err", loadErr)
		// A load failure is a server error, not a zero-match: finalise the
		// run as failed so it does not stay 'pending' forever.
		if ferr := c.store.FinalizeRunToFailed(ctx, runID); ferr != nil {
			c.log.Warn("failed to finalise push run after job creation error",
				"run", runID, "err", ferr)
		}
	}
	c.finishIfNoJobs(ctx, runID, loadErr)

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
		c.releaseSlot(repoID, orgID)
		return fmt.Errorf("controller pr: create run: %w", err)
	}
	c.holdSlot(runID, repoID, orgID)
	c.log.Info("run created", "run", runID, "number", number,
		"event", "pull_request", "pr", payload.PullRequest.Number, "repo", payload.Repository.FullName)

	// Prepare the forge check before any job exists (see HandlePush).
	c.report(ctx, owner, repoName, payload.PullRequest.Head.SHA, runID, branch)

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
	loadErr := c.LoadAndCreateJobs(ctx, repoID, runID, "pull_request", branch, payload.PullRequest.Head.SHA, payload.Action, isFork,
		baseCloneURL, checkoutRef)
	if loadErr != nil {
		c.log.Warn("job creation failed for PR run", "run", runID, "err", loadErr)
		// A load failure is a server error, not a zero-match: finalise the
		// run as failed so it does not stay 'pending' forever.
		if ferr := c.store.FinalizeRunToFailed(ctx, runID); ferr != nil {
			c.log.Warn("failed to finalise PR run after job creation error",
				"run", runID, "err", ferr)
		}
	}
	c.finishIfNoJobs(ctx, runID, loadErr)

	// Audit the run creation with the PR author's login as the actor.
	c.emitAudit(ctx, runID, "pull_request", actor, owner, repoName, branch)

	// Open a deployment-approval gate if the run requires approval.
	c.maybeOpenGate(ctx, runID, owner, repoName, payload)
	return nil
}

// HandleCheckSuite handles check_suite "rerequested" events: a user clicked
// "Re-run all checks" in GitHub's Checks UI.
//
// "requested" is ignored. GitHub sends it for every push, and HandlePush has
// already created that commit's run; acting on it would run every push twice.
func (c *Controller) HandleCheckSuite(payload webhook.CheckSuitePayload) error {
	if payload.Action != "rerequested" {
		return nil
	}
	return c.rerun(payload.Repository.FullName, payload.CheckSuite.HeadSHA, "check_suite")
}

// HandleCheckRun handles check_run "rerequested" events: a user clicked
// "Re-run" on one CICaDo check. CICaDo reports one check per run, so this
// re-runs the whole run, exactly as a check_suite re-request does.
func (c *Controller) HandleCheckRun(payload webhook.CheckRunPayload) error {
	if payload.Action != "rerequested" {
		return nil
	}
	return c.rerun(payload.Repository.FullName, payload.CheckRun.HeadSHA, "check_run")
}

// rerun creates a new run that replays the most recent run of fullName at
// sha. source names the triggering webhook event for logs and the audit trail.
//
// The new run copies the original's event, ref and fork flag rather than
// reading them from the payload. check_suite and check_run are not workflow
// trigger types, so trigger matching needs the original event; and neither
// payload carries a fork signal, so the original run's is_fork is the only
// trustworthy one. A re-run of a fork PR therefore stays a fork run, with no
// secrets and no deploy identity.
//
// A commit CICaDo never ran is skipped: there is nothing to replay, and
// guessing an event or a fork flag would be unsafe.
func (c *Controller) rerun(fullName, sha, source string) error {
	if sha == "" {
		return nil
	}
	owner, repoName, ok := splitRepo(fullName)
	if !ok {
		return fmt.Errorf("controller %s: invalid repo full_name %q", source, fullName)
	}

	ctx := context.Background()
	orgID, repoID, err := c.resolveRepo(ctx, owner, repoName)
	if err != nil {
		c.log.Warn("controller "+source+": store lookup failed",
			"repo", fullName, "err", err)
		return fmt.Errorf("controller %s: resolve repo: %w", source, err)
	}

	orig, found, err := c.store.FindRerunSource(ctx, repoID, sha)
	if err != nil {
		return fmt.Errorf("controller %s: find original run: %w", source, err)
	}
	if !found {
		c.log.Info("re-run requested for a commit with no run; skipping",
			"event", source, "repo", fullName, "sha", sha)
		return nil
	}

	if c.cfg.Backpressure != nil {
		ok, err := c.cfg.Backpressure.Acquire(ctx, repoID.String(), orgID.String())
		if err != nil || !ok {
			c.log.Debug("backpressure: skipping "+source,
				"repo", fullName, "ok", ok, "err", err)
			return fmt.Errorf("controller %s: backpressure", source)
		}
	}

	runID, number, err := c.store.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         orig.Event,
		Ref:           orig.Ref,
		Sha:           sha,
		Actor:         "github-rerequest",
		TriggerSource: store.TriggerForge,
		IsFork:        orig.IsFork,
	})
	if err != nil {
		c.releaseSlot(repoID, orgID)
		return fmt.Errorf("controller %s: create run: %w", source, err)
	}
	c.holdSlot(runID, repoID, orgID)
	c.log.Info("run created (re-run)", "run", runID, "number", number,
		"event", orig.Event, "source", source, "rerun_of", orig.RunID,
		"repo", fullName, "is_fork", orig.IsFork)

	// Prepare the forge check before any job exists (see HandlePush).
	c.report(ctx, owner, repoName, sha, runID, orig.Ref)

	// A pull_request run is matched against its workflows' pull_request
	// triggers, which need a PR action. The original action is not stored;
	// a re-run of the same head commit is replayed as "synchronize", the
	// action GitHub itself sends when a PR's head changes.
	prAction := ""
	if orig.Event == "pull_request" {
		prAction = "synchronize"
	}
	// Check out what the original run's jobs checked out (for a PR, the base
	// repo's refs/pull/<N>/head). If the original run created no jobs, fall
	// back to the repo's clone URL and the run's ref.
	cloneURL, checkoutRef := orig.CloneURL, orig.CheckoutRef
	if cloneURL == "" {
		cloneURL = "https://github.com/" + fullName + ".git"
	}
	if checkoutRef == "" && orig.Event == "push" {
		checkoutRef = "refs/heads/" + orig.Ref
	}
	loadErr := c.LoadAndCreateJobs(ctx, repoID, runID, orig.Event, orig.Ref, sha,
		prAction, orig.IsFork, cloneURL, checkoutRef)
	if loadErr != nil {
		c.log.Warn("job creation failed for re-run", "run", runID, "err", loadErr)
		// As on push and pull_request: finalise the run as failed so it
		// does not stay 'pending' forever.
		if ferr := c.store.FinalizeRunToFailed(ctx, runID); ferr != nil {
			c.log.Warn("failed to finalise re-run after job creation error",
				"run", runID, "err", ferr)
		}
	}
	c.finishIfNoJobs(ctx, runID, loadErr)
	c.emitAudit(ctx, runID, source, "github-rerequest", owner, repoName, orig.Ref)
	return nil
}

// RunCompleted completes the run's forge check with the run's final state.
// main.go calls it from runnersvc.Config.OnRunTerminal, which the queue
// fires exactly once per run, after the transaction that finished the run's
// last job has committed (CIC-103). state is "succeeded", "failed" or
// "cancelled".
//
// A run with no prepared check (NoOp forge, a failed Prepare, a run that
// predates migration 0004) is skipped: completing check 0 would only earn a
// 404 from GitHub.
func (c *Controller) RunCompleted(ctx context.Context, runID uuid.UUID, state string) error {
	if c.forge == nil {
		return nil
	}
	ref, err := c.store.GetRunCheckRef(ctx, runID)
	if err != nil {
		return fmt.Errorf("controller run completed: %w", err)
	}
	if !ref.CheckID.Valid || ref.CheckID.Int64 == 0 {
		c.log.Debug("no forge check prepared for run; skipping Complete", "run", runID)
		return nil
	}
	return c.forge.Complete(ctx, ref.CheckID.Int64, forge.RunRef{
		Org:    ref.Owner,
		Repo:   ref.Repo,
		Sha:    ref.SHA,
		RunID:  runID.String(),
		Branch: ref.Ref,
	}, conclusionFor(state), nil)
}

// finishIfNoJobs finishes a run that got no jobs: no workflow matched, no
// loader is configured, or loading failed. Such a run never finishes a job,
// so the queue never fires OnRunTerminal for it, and neither ReleaseRun nor
// RunCompleted would otherwise run: its backpressure slot would leak and its
// forge check would stay in_progress. finishIfNoJobs releases the slot and
// completes the check at once.
//
// The conclusion is the run's own terminal status when it has one (CIC-85
// finalises zero-job runs). A run still pending is reported as failed when
// loading failed and as succeeded otherwise, matching CIC-85's rule.
//
// A run with jobs is left alone: OnRunTerminal finishes it. If the job count
// cannot be read the slot is released (briefly exceeding the cap is
// recoverable, a leaked slot is not) but the check is left open, since
// completing the check of a run whose jobs are still running would report a
// result too early.
func (c *Controller) finishIfNoJobs(ctx context.Context, runID uuid.UUID, loadErr error) {
	if c.store == nil {
		return
	}
	n, err := c.store.CountRunJobs(ctx, runID)
	if err != nil {
		c.log.Warn("counting run jobs failed; releasing its slot, leaving its forge check open",
			"run", runID, "err", err)
		c.ReleaseRun(runID)
		return
	}
	if n > 0 {
		return
	}
	c.ReleaseRun(runID)
	if c.forge == nil {
		return
	}
	state := runSucceeded
	if loadErr != nil {
		state = runFailed
	}
	if run, err := c.store.GetRun(ctx, runID); err != nil {
		c.log.Warn("reading zero-job run failed", "run", runID, "err", err)
	} else if isTerminalRunStatus(run.Status) {
		state = run.Status
	}
	if err := c.RunCompleted(ctx, runID, state); err != nil {
		c.log.Warn("completing forge check for zero-job run failed", "run", runID, "err", err)
	}
}

// isTerminalRunStatus reports whether status is a finished run's status.
func isTerminalRunStatus(status string) bool {
	switch status {
	case runSucceeded, runFailed, runCancelled:
		return true
	}
	return false
}

// Run terminal statuses, as stored in runs.status and passed to RunCompleted.
const (
	runSucceeded = "succeeded"
	runFailed    = "failed"
	runCancelled = "cancelled"
)

// conclusionFor maps a run's terminal status to a forge check conclusion.
func conclusionFor(state string) forge.Conclusion {
	switch state {
	case runSucceeded:
		return forge.ConclusionSuccess
	case runCancelled:
		return forge.ConclusionCancelled
	default:
		return forge.ConclusionFailure
	}
}

// ── backpressure (CIC-106) ───────────────────────────────────────────────────

// A webhook takes a backpressure slot for its repo before creating a run, and
// the run holds it until it is terminal. The slot comes back exactly once, on
// whichever of these happens:
//
//   - CreateRun fails: releaseSlot, since no run exists to hold it;
//   - the run got no jobs (no matching workflow, or loading failed): such a
//     run never reaches a terminal job, so the queue never fires the
//     run-terminal hook, and finishIfNoJobs returns the slot at once;
//   - the run finishes: ReleaseRun, from runnersvc.Config.OnRunTerminal.
//
// holdSlot registers the run before its jobs are enqueued. A run that
// finished before it was registered would keep its slot forever.

// releaseSlot returns a slot that no run holds.
func (c *Controller) releaseSlot(repoID, orgID uuid.UUID) {
	if c.cfg.Backpressure != nil {
		c.cfg.Backpressure.Release(repoID.String(), orgID.String())
	}
}

// holdSlot hands the webhook's slot to runID until ReleaseRun.
func (c *Controller) holdSlot(runID, repoID, orgID uuid.UUID) {
	if c.cfg.Backpressure != nil {
		c.cfg.Backpressure.Register(runID.String(), repoID.String(), orgID.String())
	}
}

// ReleaseRun returns the backpressure slot runID holds. main.go calls it from
// runnersvc.Config.OnRunTerminal. It is idempotent: a second call, or a call
// for a run that holds no slot, does nothing.
func (c *Controller) ReleaseRun(runID uuid.UUID) {
	if c.cfg.Backpressure != nil {
		c.cfg.Backpressure.ReleaseByRunID(runID.String())
	}
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
	// webhook receiver provides the App installation ID (CIC-81).
	return c.store.ResolveRepo(ctx, owner, repoName, c.cfg.DefaultBranch, "")
}

func splitRepo(fullName string) (owner, repo string, ok bool) {
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// report prepares the run's check on the forge and records the returned
// check ID on the run, so RunCompleted can complete that same check. Both
// steps are non-fatal: without a recorded ID the run still executes, and
// RunCompleted skips it.
func (c *Controller) report(ctx context.Context, owner, repo, sha string, runID uuid.UUID, branch string) {
	if c.forge == nil {
		return
	}
	checkID, ok := report(ctx, c.forge, owner, repo, sha, runID, branch, c.log)
	if !ok || c.store == nil {
		return
	}
	if err := c.store.SetPreparedCheckID(ctx, runID, checkID); err != nil {
		c.log.Warn("recording forge check id failed", "run", runID, "err", err)
	}
}

// report calls forge.Prepare for a new run. ok is false when Prepare failed.
func report(
	ctx context.Context,
	f forge.Forge,
	owner, repo, sha string,
	runID uuid.UUID,
	branch string,
	log *slog.Logger,
) (checkID int64, ok bool) {
	t := time.Now()
	checkID, err := f.Prepare(ctx, forge.RunRef{
		Org:     owner,
		Repo:    repo,
		Sha:     sha,
		RunID:   runID.String(),
		Branch:  branch,
		Started: &t,
	})
	if err != nil {
		log.Warn("forge.Prepare failed", "run", runID, "err", err)
		return 0, false
	}
	return checkID, true
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

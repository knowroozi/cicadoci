// CIC-71c: workflow → job DAG creation
//
// This file defines the WorkflowLoader seam and connects
// workflow.Workflow (parsed YAML) to store.CreateJobSpecs (the DAG
// writer).  The controller calls LoadAndCreateJobs after a run row
// is created; LoadAndCreateJobs loads the repo's workflows,
// matches each against the triggering event, and creates a job DAG
// for every matching workflow.
//
// The WorkflowLoader interface lets different backends (static in-memory
// for tests, store-based for the web UI, and eventually GitHub-API-based
// for production) plug in without changing the controller's API.
//
// WorkflowLoader is nil in DefaultConfig; a nil loader means
// LoadAndCreateJobs is a silent no-op — runs are created but have
// no jobs.  This is intentional backwards-compatibility with
// CIC-71a/CIC-71b.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/secretexpr"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/workflow"
	"cicadoci.com/cicado/server/workflow/parse"

	"sort"
)

// ── WorkflowLoader interface ──────────────────────────────────────────────────

// WorkflowLoader loads workflow definitions for a repository.
//
// Different backends implement this interface:
//
//   - StaticLoader  — pre-parsed in-memory registry (tests, dev servers).
//   - StoreLoader   — reads YAML from the workflows table by repo_id.
//
// In production the CIC-84 producer fetches the workflow files at the
// event SHA and upserts them, and StoreLoader reads them back.
//
// The controller calls LoadAll once per run; it does not cache.
type WorkflowLoader interface {
	// LoadAll returns all workflow definitions registered for repoID.
	//
	// If sha is non-empty, only workflows known at that revision are
	// returned (git-aware loader).  If sha is empty, the most recent
	// known definitions are returned.
	//
	// A nil return with no error means the repo has no workflows.
	// This is not an error — the caller should log a debug event.
	LoadAll(ctx context.Context, repoID uuid.UUID, sha string) ([]*workflow.Workflow, error)
}

// ── StaticLoader ──────────────────────────────────────────────────────────────

// StaticLoader loads workflows from an in-memory registry.
//
// Used by tests and by pre-seeded development servers where the
// operator has pre-loaded a fixed set of workflows.
//
// Registration is done in-process via Register; the loader is not
// concurrent-safe and should be read-only after all Register calls
// complete (typically before the server begins accepting webhook
// events).
type StaticLoader struct {
	workflows map[uuid.UUID][]*workflow.Workflow
}

// NewStaticLoader returns an empty StaticLoader.
func NewStaticLoader() *StaticLoader {
	return &StaticLoader{workflows: make(map[uuid.UUID][]*workflow.Workflow)}
}

// Register adds a parsed workflow to the static registry for repoID.
//
// Callers must ensure the workflow has been validated (workflow.Validate()
// or workflow/parse) before registering.
func (l *StaticLoader) Register(repoID uuid.UUID, w *workflow.Workflow) {
	l.workflows[repoID] = append(l.workflows[repoID], w)
}

// LoadAll returns all workflows registered for repoID.
// sha is ignored: StaticLoader is not revision-aware.
func (l *StaticLoader) LoadAll(_ context.Context, repoID uuid.UUID, _ string) ([]*workflow.Workflow, error) {
	return l.workflows[repoID], nil
}

// ── StoreLoader ────────────────────────────────────────────────────────────────

// WorkflowLister is the narrow interface StoreLoader needs from the store.
//
// It decouples LoadAll from the concrete *store.Store so it can be
// unit-tested without a real Postgres connection.
//
// *store.Store satisfies it via ListWorkflowsForRepo.
type WorkflowLister interface {
	ListWorkflowsForRepo(ctx context.Context, repoID uuid.UUID) ([]store.WorkflowRow, error)
}

// StoreLoader loads workflow YAML from the workflows table.
//
// It reads the workflows repo has registered and decodes each row's
// Definition jsonb into a *workflow.Workflow.  Malformed rows are skipped
// with a warning; other valid workflows are still returned.
type StoreLoader struct {
	store WorkflowLister
	log   *slog.Logger
}

// NewStoreLoader wraps a *store.Store (which satisfies WorkflowLister).
func NewStoreLoader(st WorkflowLister, log *slog.Logger) *StoreLoader {
	if log == nil {
		log = slog.Default()
	}
	return &StoreLoader{store: st, log: log}
}

// compile-time check: *store.Store satisfies WorkflowLister.
var _ WorkflowLister = (*store.Store)(nil)

// LoadAll reads workflows for repoID from the store.
//
// sha is ignored in this MVP; workflows are loaded at the latest
// version in the table.  A future revision-aware loader would read
// the workflow version pinned to the run's SHA.

// LoadAll reads workflow rows for repoID from the store and decodes
// each row's Definition jsonb into a *workflow.Workflow.
//
// sha is ignored in this MVP; the store returns the most-recent version
// of each workflow path.  A future revision-aware loader would read the
// workflow pinned to the run's SHA.
//
// Malformed rows are skipped with a warning; other valid workflows are
// still returned.
func (l *StoreLoader) LoadAll(ctx context.Context, repoID uuid.UUID, _ string) ([]*workflow.Workflow, error) {
	rows, err := l.store.ListWorkflowsForRepo(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("StoreLoader.LoadAll: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	var out []*workflow.Workflow
	for _, row := range rows {
		w := &workflow.Workflow{}
		if len(row.Definition) > 0 {
			if err := json.Unmarshal(row.Definition, w); err != nil {
				l.log.Warn("StoreLoader: failed to decode workflow definition; skipping",
					"repo", repoID, "path", row.Path, "err", err)
				continue
			}
		}
		// Fall back to the row's Name if the decode didn't populate it.
		if w.Name == "" {
			w.Name = row.Name
		}
		out = append(out, w)
	}
	return out, nil
}

// ── Workflow→JobSpec conversion ───────────────────────────────────────────────

// convertJobSpecs converts a parsed workflow into a slice of store.JobSpec
// suitable for store.CreateJobSpecs.
//
// Job specs are returned in topological order: the DAG's Levels
// determine the ordering; within the same level, names are sorted
// alphabetically.
//
// Each job's Needs entry (workflow.Job.Needs) is passed directly to
// JobSpec.Needs.  CreateJobSpecs resolves the names to UUIDs inside a
// transaction and fails if any referenced name is not in the same list.
//
// gitCloneURL, gitRef, gitSha are the checkout coordinates for the runner.
// isFork is true for fork PRs and gates secret access via secretRefsFor.
func convertJobSpecs(w *workflow.Workflow, dag *workflow.DAG, gitCloneURL, gitRef, gitSha string, isFork bool) []store.JobSpec {

	specs := make([]store.JobSpec, 0, len(dag.Jobs))

	for _, name := range dag.Jobs {
		j, ok := w.Jobs[name]
		if !ok {
			// DAG.Jobs guarantees these exist; this is defensive.
			continue
		}

		// CIC-148: when this job runs a workflow with git context, prepend a
		// synthetic "checkout" step so the runner clones the repo before the
		// first user step.  Without this, a workflow expecting the repo in the
		// workdir always sees an empty directory.
		steps := convertSteps(j.Steps)
		if gitCloneURL != "" {
			steps = append([]store.StepSpec{cloneStepSpec(gitCloneURL, gitRef, gitSha, isFork)}, steps...)
		}
		spec := store.JobSpec{
			Name:         name,
			Executor:     "cicanscript", // TODO(CIC-132): select executor from workflow step spec, not hard-coded
			RequiredTags: j.RunsOn,
			TimeoutSec:   j.TimeoutSeconds,
			Needs:        []string(j.Needs),
			MaxAttempts:  3,
			Priority:     100,
			Env:          envToJSON(j.Env),
			SecretRefs:   secretRefsFor(j, isFork, gitCloneURL),
			GitCloneURL:  gitCloneURL,
			GitRef:       gitRef,
			GitSha:       gitSha,
			Steps:        steps,
		}
		specs = append(specs, spec)
	}

	return specs
}

// envToJSON converts a map[string]string to JSON bytes for the job env column.
//
// Returns nil when env is empty so the store stores an empty object "{}"
// (the defaultInt-like behaviour in createJobSpecs).
func envToJSON(env map[string]string) []byte {
	if len(env) == 0 {
		return nil // store defaults to {} for empty env
	}

	// Use encoding/json for correctness — env keys are not user-controlled
	// in MVP but could be in the future, and json.Marshal handles escaping
	// properly.
	// json.Marshal on a map[string]string cannot fail; the value is a plain
	// Go value with no custom marshaller, so we can safely ignore the error.
	b, _ := json.Marshal(env)
	return b
}

// gitCredentialHelper is the git credential helper string used to
// inject CICADO_GITHUB_TOKEN into git's HTTP credential flow (CIC-148 B3).
//
// Exposed as a package constant so that B3 tests can drive
// `git -c credential.helper=<const> credential fill` without
// parsing the generated script.
//
// The `!f() { ... }; f` syntax tells git to run /bin/sh -c on the
// helper text, sourcing the function and calling it.  The
// password field reads CICADO_GITHUB_TOKEN from the step env.
const gitCredentialHelper = `!f() { echo username=x-access-token; echo password="$CICADO_GITHUB_TOKEN"; }; f`

// githubCloneTokenSecretName is the secrets-store key under which a GitHub
// installation access token (used to clone a private repo) is stored by the
// operator and fetched via GetJobSecrets, injected into the runner's
// CICADO_GITHUB_TOKEN step env, and consumed by the git credential helper
// in buildCloneScript.  The token value is NEVER stored on the step, in
// the job row's `env` column, or in the clone URL; it flows to the runner
// only via the lease-scoped GetJobSecrets RPC and is registered in the
// redaction masker so it never appears in job logs.
const githubCloneTokenSecretName = "github_clone_token"

// secretRefsFor returns the distinct, sorted list of secret NAMES a job
// references, extracted from the job-level env AND every step's env.  Each
// step's env is evaluated independently so two steps that happen to use the
// same key with different secret refs both contribute their name.  Secret
// VALUES are never stored here; only the NAME list is, persisted in
// `secret_refs` and authorized by GetJobSecrets against the live lease.
//
// CIC-148 / CIC-151: when gitCloneURL is non-empty (job has a git context),
// `github_clone_token` is added so the server can deliver a per-repo
// GitHub installation token to the runner via GetJobSecrets. The token is
// minted at lease time by runnersvc.CloneMinterFunc. Fork runs get NO
// other secrets unconditionally — the defense-in-depth backstop on top of
// ForkPolicy.Apply: `secretRefsFor` returns nil static-secret refs for fork
// jobs regardless of the workflow text.
func secretRefsFor(j *workflow.Job, isFork bool, gitCloneURL string) []string {
	seen := make(map[string]struct{})
	// CIC-148 / CIC-151: when the job has a git context, the runner needs
	// a per-repo GitHub installation token for the checkout step.
	// The token is minted at lease time by runnersvc.CloneMinterFunc
	// and delivered via GetJobSecrets. Fork runs get the fork carve-out
	// applied to all OTHER static-secret refs (CIC-27 defense-in-depth).
	if gitCloneURL != "" {
		seen[githubCloneTokenSecretName] = struct{}{}
	}
	// Skip static-secret refs from the user workflow for fork runs; only
	// github_clone_token is allowed for fork PR jobs.
	if !isFork {
		for _, n := range secretexpr.Refs(j.Env) {
			seen[n] = struct{}{}
		}
		for _, step := range j.Steps {
			for _, n := range secretexpr.Refs(step.Env) {
				seen[n] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// cloneStepSpec builds a synthetic store.StepSpec that fetches and checks out
// the commit to build as the first step of a job when git context is available.
//
// CIC-148 + CIC-151: CICADO_GITHUB_TOKEN is injected for ALL clone steps
// (fork and non-fork).  Fork PR runs get a contents:read token via the
// clone minter; all other secret refs stay refused by GetJobSecrets
// (CIC-27 defense-in-depth).  The token value is NEVER stored in the step
// command or the job row's env column; it is fetched via GetJobSecrets
// and injected here as a secret template reference.
//
// CIC-147: without this, JobAssignment.Git was ignored by the runner and every
// workflow step ran in an empty workdir.  We emit one shell command that runs
// the whole checkout so the output streams directly to the job logs via the
// normal RunStep path.
//
// Credentials: the URL is a plain https clone URL with no token in it.  The
// token is stored as a job secret (github_clone_token) and reached via
// GetJobSecrets RPC — NEVER embedded in Command (the steps table is
// readable by all runners, so a token there would leak on any job
// assignment).  See CLAUDE.md "Security invariants".
func cloneStepSpec(cloneURL, ref, sha string, isFork bool) store.StepSpec {
	spec := store.StepSpec{
		Name:    "checkout",
		Command: []string{"/bin/sh", "-ec", buildCloneScript(cloneURL, ref, sha)},
	}
	// CIC-148 + CIC-151 B1: CICADO_GITHUB_TOKEN for ALL clone steps.
	// Fork PR runs get a contents:read token via the minter.
	if cloneURL != "" {
		spec.Env = []byte(`{"CICADO_GITHUB_TOKEN":"${{ secrets.github_clone_token }}"}`)
	}
	return spec
}

// buildCloneScript generates the POSIX /bin/sh script for the checkout step:
//
//	git init -q .
//	git remote add origin <url>
//	git credential.helper '!f() { echo username="x-access-token"; echo password="$CICADO_GITHUB_TOKEN"; }; f'
//	git fetch --depth 1 origin <sha, else ref>
//	git checkout -q FETCH_HEAD
//
// CIC-148: if the step's env contains CICADO_GITHUB_TOKEN (injected by the
// controller for non-fork runs with a GitHub context, resolved by the
// runner via GetJobSecrets), the clone uses it as a credential via
// `git credential.helper`.  For fork PR runs the step has no
// CICADO_GITHUB_TOKEN; the credential-helper line still runs but the
// password field will be empty, and the fetch will fail on private
// repos — this is the desired behaviour (fork runs get no secrets).
//
// It fetches exactly the commit to build instead of cloning the default
// branch: `git clone --depth 1` implies `--single-branch`, so a later
// `git checkout <ref>` of any non-default branch or PR ref fails.
// Every argument is single-quoted via shellQuote to prevent shell injection.
func buildCloneScript(url, ref, sha string) string {
	target := sha
	if target == "" {
		target = ref
	}
	if target == "" {
		target = "HEAD"
	}
	// CIC-148 N1+B2: build the conditional clone script.  N1: git -c
	// credential.helper (not git config).  B2: guard on CICADO_GITHUB_TOKEN
	// being non-empty so that absent/whitespace tokens fall through to the
	// anonymous git fetch path.  fetchSubcmd omits the leading git so that
	// the then-branch is git -c ... fetch ... (not git ... git ... fetch ...).
	fetchSubcmd := "fetch --depth 1 origin " + shellQuote(target)
	var sb strings.Builder
	sb.WriteString("git init -q .\n")
	sb.WriteString("git remote add origin " + shellQuote(url) + "\n")
	sb.WriteString("if [ -n \"$CICADO_GITHUB_TOKEN\" ]; then\n")
	sb.WriteString("\tgit -c credential.helper='" + gitCredentialHelper + "' " + fetchSubcmd + "\n")
	sb.WriteString("else\n")
	sb.WriteString("\tgit " + fetchSubcmd + "\n")
	sb.WriteString("fi\n")
	sb.WriteString("git checkout -q FETCH_HEAD\n")
	return sb.String()
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes via
// the POSIX '\” idiom.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// convertSteps maps workflow.Step → store.StepSpec for each step in the job.
func convertSteps(steps []*workflow.Step) []store.StepSpec {
	if len(steps) == 0 {
		return nil
	}
	out := make([]store.StepSpec, len(steps))
	for i, s := range steps {
		name := s.Name
		if name == "" {
			name = s.Uses
		}
		if name == "" && s.Run != "" {
			// Use first word of Run as step name.
			name = s.Run
		}
		var cmd []string
		if s.Run != "" {
			// CIC-147: wrap the script as /bin/sh -ec <script> so a
			// multi-word or multi-line run: string executes correctly
			// instead of failing as "executable file not found" when
			// the exec package treats the whole string as argv[0].
			// Both the shell and docker executors consume step.Command
			// as trailing argv, so this 3-element slice works for both.
			cmd = []string{"/bin/sh", "-ec", s.Run}
		}
		envJSON, _ := json.Marshal(s.Env) // nil or empty → {}
		out[i] = store.StepSpec{
			Name:           name,
			Image:          s.Image,
			Command:        cmd,
			Env:            envJSON,
			TimeoutSeconds: s.TimeoutSeconds,
		}
	}
	return out
}

// ── controller integration ──────────────────────────────────────────────────────

// LoadAndCreateJobs loads all workflows for repoID, matches each against
// the triggering event, and creates a job DAG for every matching workflow.
//
// Called after CreateRun returns successfully.  Failures in loading,
// parsing, or DAG creation for individual workflows are logged but do
// not propagate — a single broken workflow should not block other
// workflows in the repo.
//
// The cfg.workflowLoader may be nil; a nil loader is a silent no-op.
//
// eventType: "push" or "pull_request"
// ref:       branch name (for push) or base ref (for PR)
// eventSHA:  commit SHA that triggered the event
// prAction:  "opened", "synchronize" or "reopened" for PR events; "" for push
// isFork is true when the PR event originates from a fork repo; when true
// and c.cfg.ForkPolicy is non-nil, fork restrictions are applied.
func (c *Controller) LoadAndCreateJobs(
	ctx context.Context,
	repoID uuid.UUID,
	runID uuid.UUID,
	eventType, ref, eventSHA, prAction string,
	isFork bool,
	gitCheckoutURL, gitCheckoutRef string,
) error {
	// gitCheckoutURL is the clone URL for the checkout ref;
	// gitCheckoutRef is the actual git ref to check out
	// (e.g. "refs/pull/N/head" for PRs, "refs/heads/main" for pushes).
	// The trigger-matching ref (the 'ref' param) is separate and
	// comes from the base branch for PRs.
	loader := c.cfg.WorkflowLoader
	if loader == nil {
		// No loader configured: treat as zero-match and finalise the run.
		// Only attempt finalisation when the store is wired; in unit tests
		// where store is nil the run may stay pending, which is acceptable.
		if c.store != nil {
			if ferr := c.store.FinalizeRunToSucceeded(ctx, runID); ferr != nil {
				c.log.Warn("failed to finalise no-loader run", "run", runID, "repo", repoID, "err", ferr)
			}
		}
		c.log.Debug("no workflow loader; run finalised or skipped",
			"run", runID, "repo", repoID)
		return nil
	}

	workflows, err := loader.LoadAll(ctx, repoID, eventSHA)
	if err != nil {
		c.log.Warn("workflow loader failed", "run", runID, "err", err)
		return fmt.Errorf("load workflows: %w", err)
	}

	if len(workflows) == 0 {
		// CIC-85: a run with no stored workflows still needs a terminal state.
		if err := c.store.FinalizeRunToSucceeded(ctx, runID); err != nil {
			c.log.Warn("failed to finalise no-workflows run",
				"run", runID, "repo", repoID, "err", err)
		}
		c.log.Info("no workflows found; run finalised as succeeded",
			"run", runID, "repo", repoID)
		return nil
	}

	// Build the event that trigger matching expects.
	event := workflow.Event{
		Type:  eventType,
		Ref:   ref,
		Paths: nil, // path filters not available from GitHub webhook payload in MVP
	}
	if prAction != "" {
		event.PR = &workflow.PullRequestEvent{
			Action: prAction,
			// Branch is the PR's *base* ref; for a fork PR that's
			// the branch the PR targets.
			Branch: ref,
		}
	}

	created := 0
	matched := false // a workflow matched the event, regardless of job creation outcome
	for _, wf := range workflows {
		// Fork PRs: enforce zero-secrets / no-identity / safe-tag policy.
		if isFork && c.cfg.ForkPolicy != nil {
			wf = c.cfg.ForkPolicy.Apply(wf)
			c.log.Debug("applied fork policy to workflow", "workflow", wf.Name, "run", runID)
		}
		// A workflow that matches the event counts as "matched"
		// regardless of whether it passes validation — a matched
		// but invalid workflow means the run was expected to do work.
		if !wf.On.Matches(event) {
			continue
		}
		matched = true

		// Validate the workflow before attempting DAG creation.
		// A workflow loaded from an unknown source may be structurally
		// invalid; we skip it and log a warning rather than failing the run.
		if verr := wf.Validate(); verr != nil {
			c.log.Warn("workflow validation failed; skipping",
				"workflow", wf.Name, "run", runID, "err", verr)
			continue
		}

		dag, err := workflow.BuildDAG(wf)
		if err != nil {
			c.log.Warn("DAG build failed; skipping workflow",
				"workflow", wf.Name, "run", runID, "err", err)
			continue
		}

		specs := convertJobSpecs(wf, dag, gitCheckoutURL, gitCheckoutRef, eventSHA, isFork)
		if len(specs) == 0 {
			// Valid workflow with no jobs — skip.
			continue
		}

		_, err = c.store.CreateJobSpecs(ctx, runID, specs)
		if err != nil {
			c.log.Warn("job creation failed; skipping workflow",
				"workflow", wf.Name, "run", runID, "err", err)
			continue
		}

		created++
		c.log.Info("workflow jobs created",
			"workflow", wf.Name, "run", runID, "job_count", len(specs))
	}

	if created == 0 {
		// CIC-85: zero-match run finalisation.
		if matched {
			// A workflow matched the event but produced no jobs
			// (DAG build failure, validation error, or store insert error).
			// The run was expected to do work; finalise as failed.
			if err := c.store.FinalizeRunToFailed(ctx, runID); err != nil {
				c.log.Warn("failed to finalise matched-but-failed run",
					"run", runID, "repo", repoID, "err", err)
			}
			c.log.Warn("matched workflow produced no jobs; run finalised as failed",
				"run", runID, "repo", repoID, "event_type", eventType)
		} else {
			// No workflow matched the event: flip to succeeded.
			if err := c.store.FinalizeRunToSucceeded(ctx, runID); err != nil {
				c.log.Warn("failed to finalise zero-match run",
					"run", runID, "repo", repoID, "err", err)
			}
			c.log.Info("no workflows matched the event; run finalised as succeeded",
				"run", runID, "repo", repoID, "event_type", eventType)
		}
	}
	return nil
}

// ── Controller Config extension ──────────────────────────────────────────────────

// The Config struct in controller.go has WorkflowLoader *WorkflowLoader.
// It's nil in DefaultConfig, so a controller with no loader wired is
// backwards-compatible: runs are created, jobs are not.
//
// The controller's LoadAndCreateJobs method checks c.cfg.WorkflowLoader
// before calling LoadAll.
//
// ── Parse helper ────────────────────────────────────────────────────────────────

// ParseWorkflowBytes parses YAML bytes into a workflow.Workflow.
//
// It is exported so that an operator or test can register a workflow
// with StaticLoader before starting the server.
//
// Returns (*workflow.Workflow, error) where err is non-nil if parsing
// failed. The error carries positional information via *parse.ParseErrors.
func ParseWorkflowBytes(src []byte, fileName string) (*workflow.Workflow, error) {
	wf, perr, err := parse.Parse(src, fileName)
	if err != nil {
		return nil, err
	}
	if perr != nil {
		return nil, perr
	}
	return wf, nil
}

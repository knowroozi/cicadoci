// Package workflow defines the YAML workflow schema v1: the Go types that
// the parser produces and the JSON Schema that external tools and agent
// MCP consumers use as their contract.
//
// The schema is deliberately constrained: no loops, no expression language,
// no templating. Logic lives in scripts and containers (Jenkins Groovy lesson).
// Only `if:` on step status and branch/tag filters exist as conditionals.
package workflow

// Version is the schema version this package implements.
//
// It doubles as the major-version discriminator a future JSON-Schama consumer
// can use to branch on behaviour. Bumping this is a breaking change and must be
// a new major version with a new `--schema-version` CLI flag.
const Version = 1

// Workflow is the top-level structure of a CICaDo workflow file.
//
// It represents a single workflow document; a repository can host multiple
// workflow files, each one of which is independently parsed and validated.
type Workflow struct {
	// Name is the human-readable workflow name.
	//
	// Shown in the UI, Slack messages, and the API. Must be non-empty and
	// unique within a repository.
	Name string `yaml:"name" json:"name"`

	// On selects which events trigger this workflow.
	On On `yaml:"on" json:"on"`

	// Jobs is the map of job-name → job-spec. At least one job is required.
	Jobs Jobs `yaml:"jobs" json:"jobs"`
}

// On is the trigger selection for a workflow.
//
// A workflow fires when any of the listed trigger types matches. All three
// trigger types are optional; at least one must be set for the workflow to
// be valid.
type On struct {
	// Push fires on git push events.
	Push *PushTrigger `json:"push,omitempty"`

	// PullRequest fires on pull-request open/synchronize/reopen events.
	PullRequest *PullRequestTrigger `json:"pull_request,omitempty"`

	// Schedule fires on a cron schedule.
	//
	// Schedule-only workflows do not fire on manual / UI triggers in M1.
	Schedule *ScheduleTrigger `json:"schedule,omitempty"`

	// Additional holds any trigger sub-maps not explicitly typed here.
	// This allows forward compatibility for new trigger types without a
	// schema change. The map key is the trigger type name (e.g. "cron" if
	// someone adds it in the future). In M1 this is reserved/unused.
	Additional map[string]any `json:"-"`
}

// PushTrigger fires when commits are pushed to matching refs.
type PushTrigger struct {
	// Branches is a list of glob patterns matching branch names.
	//
	// An empty list means "all branches".
	//
	// Patterns support `*`, `**`, and `?` (same rules as gitignore).
	Branches []string `yaml:"branches,omitempty" json:"branches,omitempty"`

	// Tags is a list of glob patterns matching tag names.
	Tags []string `yaml:"tags,omitempty" json:"tags,omitempty"`

	// Paths is a list of pathspecs; the workflow only fires if at least one
	// changed file matches at least one pathspec.
	//
	// Uses git's pathspec syntax. Empty or absent means "all paths".
	Paths []string `yaml:"paths,omitempty" json:"paths,omitempty"`
}

// PullRequestTrigger fires on pull-request events.
type PullRequestTrigger struct {
	// Branches restricts which target branches trigger the workflow.
	Branches []string `yaml:"branches,omitempty" json:"branches,omitempty"`

	// Tags restricts which target tags trigger the workflow.
	Tags []string `yaml:"tags,omitempty" json:"tags,omitempty"`

	// Types is the list of event types to handle.
	//
	// Known values: "opened", "synchronize", "reopened".
	// An empty slice means all types.
	Types []string `yaml:"types,omitempty" json:"types,omitempty"`
}

// ScheduleTrigger fires on a cron schedule.
//
// Cron follows the GitHub Actions / crontab syntax (5 fields: min hr dom mon dow).
// Timezone is UTC.
type ScheduleTrigger struct {
	// Cron is a list of standard 5-field cron expressions.
	//
	// Multiple entries are OR'd: the workflow fires if ANY expression matches.
	Cron []string `yaml:"cron" json:"cron"`
}

// Jobs is a map of job-name → job specification.
//
// The map key is the job's name, used for `needs` dependencies and for the
// UI. Job names must be unique within a workflow.
type Jobs map[string]*Job

// Job is a single job within a workflow.
type Job struct {
	// RunsOn lists the labels/tags a runner must carry to be eligible for this
	// job.
	//
	// All labels must be present on the runner; the claim query uses
	// required_tags <@ runner.labels.
	RunsOn []string `yaml:"runs-on,omitempty" json:"runs_on,omitempty"`

	// Needs lists the names of upstream jobs that must all succeed before this
	// job becomes eligible for `queued`.
	//
	// A single string is equivalent to a one-element slice.
	// The DAG is stored in `job_dependencies` at run-creation time.
	Needs Needs `yaml:"needs,omitempty" json:"needs,omitempty"`

	// Steps is the ordered list of step specifications.
	//
	// Steps run sequentially. A step's failure stops subsequent steps
	// (unless a later step has `if: always`).
	Steps []*Step `yaml:"steps" json:"steps"`

	// Approval, if set, blocks this job until a human approves it via the
	// configured chat channel.
	//
	// Approval is a pre-run gate: the job stays in `pending` state until
	// approval is granted, at which point it moves to `queued`.
	//
	// This is nil for jobs without an approval gate.
	Approval *ApprovalGate `yaml:"approval,omitempty" json:"approval,omitempty"`

	// Default image for all steps in this job.
	//
	// A step's own `image` field overrides this. The job-level image is
	// resolved at job-creation time; step-level images are resolved per-step
	// by the runner.
	Image string `yaml:"image,omitempty" json:"image,omitempty"`

	// Env is the environment map applied to every step in this job.
	//
	// A step's own env map takes precedence over the job's env.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`

	// TimeoutSeconds is the total wall-clock budget for the entire job
	// (all steps combined). 0 means "no timeout" (the runner imposes a
	// hard ceiling from server config).
	//
	// This is distinct from per-step timeouts: `Steps[i].TimeoutSeconds`
	// limits one step.
	TimeoutSeconds int `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Needs is a list of upstream job names that must all succeed before this job
// runs.
//
// It is the YAML representation of a DAG edge list: when run-creation inserts
// `job_dependencies` rows, one row is created per `Needs` entry with
// `job_id` = current job and `depends_on_job_id` = the named upstream.
//
// A self-reference (a job that lists itself) is a validation error.
type Needs []string

// Step is a single step within a job.
type Step struct {
	// Name gives the step a human-readable label shown in the UI.
	//
	// If empty, the step is identified by its `uses` or first word of `run`.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`

	// Uses is the name of a built-in action or a fully-qualified package
	// reference.
	//
	// Built-in steps in M1: `checkout`, `run` (when `run` is set, `uses`
	// must be nil — they are mutually exclusive).
	//
	// In M1, built-in step names are:
	//
	//	- "checkout": clone the repo (uses the per-job installation token)
	//	- "run":      execute the `Run` string (this is a no-op alias for `Run`)
	//
	// Marketplace/action-package `uses` (e.g. `actions/checkout@v4`) are not
	// supported in M1 — the step will fail at parse/validation time with a
	// clear message.
	Uses string `yaml:"uses,omitempty" json:"uses,omitempty"`

	// Run is the shell command executed by the runner.
	//
	// `Run` and `Uses` are mutually exclusive. When `Run` is set, `Uses`
	// must be nil. The command is executed with the step's resolved image.
	Run string `yaml:"run,omitempty" json:"run,omitempty"`

	// Image is the container image for this step.
	//
	// An empty string means "use the job-level image".
	//
	// Images should be digest-pinned where provided by the user; the runner
	// stores the resolved digest in the `steps.image` column for audit.
	Image string `yaml:"image,omitempty" json:"image,omitempty"`

	// Identity selects the cloud identity this step runs with.
	//
	// Known values in M1:
	//
	//	- "" or "none": no cloud identity (default)
	//	- "aws": OIDC → AssumeRoleWithWebIdentity using env.AWSTOKEN_ROLE_ARN
	//
	// The runner uses this to determine which OIDC role to assume at step
	// start; the actual credential exchange is M5 work.
	Identity string `yaml:"identity,omitempty" json:"identity,omitempty"`

	// Env is per-step environment overrides merged on top of the job-level
	// Env.
	//
	// Secret references (e.g. `secrets.DEPLOY_ROLE`) are resolved by the
	// runner at step start via the lease-scoped `GetJobSecrets` call.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`

	// If selects when this step runs conditionally.
	//
	// Known values (lowercase, exact match, no expressions):
	//
	//	- "success": run only if all prior steps succeeded (default)
	//	- "failure": run only if a prior step failed
	//	- "always" : run regardless of prior step outcomes
	//
	// No expression language. This is a deliberate constraint.
	If string `yaml:"if,omitempty" json:"if,omitempty"`

	// TimeoutSeconds is the wall-clock budget for this single step.
	//
	// 0 means "use the job timeout or server default".
	TimeoutSeconds int `yaml:"timeout,omitempty" json:"timeout,omitempty"`

	// NetworkEnabled, when true, opts the step into outbound network access.
	//
	// Default is false (docker executor runs `--network none`). In M1 this
	// is advisory: the runner's docker executor enforces it; the shell
	// executor on trusted infra is unaffected.
	NetworkEnabled bool `yaml:"network,omitempty" json:"network,omitempty"`
}

// ApprovalGate blocks a job until a human on the allow-list approves it.
type ApprovalGate struct {
	// Channel is the chat channel (or DM) to post the approval request to.
	//
	// In M1 this is a Slack channel name (including `#`).
	//
	// The gate posts an interactive Block Kit message; the button click
	// MUST authorize the *acting user* against the Allow list — a button
	// is clickable by anyone in the channel, so the handler itself is the
	// authz gate.
	Channel string `yaml:"channel" json:"channel"`

	// Allow lists the principals permitted to approve.
	//
	// Format: a list of `kind:name` strings where kind is one of
	// `user`, `team`, `group`.
	//
	// Example: `["user:@alice", "team:platform"]`.
	Allow []string `yaml:"allow" json:"allow"`

	// TimeoutSeconds is how long to wait for a human response before
	// auto-failing the gate.
	//
	// 0 means "no timeout — wait indefinitely" (the job stays in
	// pending state until approved).
	TimeoutSeconds int `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Validate returns an error if the Workflow has structural problems that can
// be detected without a database connection. This is a fast, in-memory
// check; it does not resolve `needs` edges (DAG cycle detection is a
// separate concern handled by the parser at CIC-21).
//
// Validated invariants:
//   - Name is non-empty
//   - At least one trigger is set
//   - Exactly one job map entry is at least `len(jobs) >= 1`
//   - Each job has at least one step
//   - Each step has `uses XOR run` set
//   - `if` values are in {success, failure, always}
func (w *Workflow) Validate() error {
	if err := Validate(w); err != nil {
		return err
	}
	return nil
}

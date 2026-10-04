# CIC-161 — Runner consumes `JobSpec.Git`: inject `CICADO_GIT_*` env vars

**Decision ref**: `docs/decisions/CIC-161-git-env-context.md`
**PR**: `knowroozi/cic-161-git-env-context` (#121)
**Status**: 2026-10-03 — implemented, awaiting Dwight review (PR not yet merged).

## Context

Found by Creed QC `@7d8a26a`. The runner protocol message
`JobAssignment` carries a `JobSpec` whose `Git` sub-message contains
`CloneUrl`, `Ref`, and `Sha` — populated by
`server/runnersvc/session.go` from the job's `queue.Job` fields,
originally resolved in `server/controller/controller.go` at webhook-time
from `payload.Repository.CloneURL` / `payload.Ref` / `payload.Sha`.

Before this fix the **runner side never read `JobSpec.Git`**. The
`job.Spec.GetGit()` call site in `runner/job.go` did not exist;
`Job.Env` and every step's env contained only what the workflow author
supplied via `job.Env` / `step.env`, plus secret-resolved values.
A cicanscript `run:` block could not reference the repository coordinate
that the server already resolved and shipped over the wire.

CIC-147 (#112) fixed the multi-word execution + `uses:`-rejection bug
and added a controller-side synthetic `git fetch` step to the step
list, but it did not make the coordinates accessible as env vars to
arbitrary `run:` blocks.

## Decision

Inject `CICADO_GIT_CLONE_URL`, `CICADO_GIT_REF`, `CICADO_GIT_SHA`
as job-level env vars in `runner/job.go` after the secrets fetch but
before `exec.Prepare`, so that every step of the job inherits them.
An operator-supplied `CICADO_GIT_*` key is preserved (do not clobber
an already-present key). Empty fields from an absent
`JobSpec.Git` are skipped (a job that did not come from a git webhook
gets no git env vars).

### Rationale

1. **Run-coordinates, not clone auth.** Clone URL + ref + SHA carry
   no credential. CIC-161 injects these three coordinates as plain env
   vars. *Clone authentication for private repos* (a per-job
   installation access token) is **not yet implemented** -- CIC-148
   is the tracking ticket; until it lands, a job running against a
   private repo has no credential and will fail.
2. **Not clobbering operator-supplied env.** `InjectGitEnv` skips a
   key when it is already present, following the same "operator
   overrides" principle as the secret-resolution layer.
3. **Not `JobSpec.Git` on the executor.** `runner/executor/Job` is
   the execution context passed to the executor; adding
   coordination there would leak the proto type into that interface.
   `Job.InjectGitEnv(clone, ref, sha string)` takes three plain
   strings and mutates `Job.Env`, keeping the executor package
   proto-free.

## Changes

### 1. `runner/executor/executor.go` — `Job.InjectGitEnv`

```go
func (j *Job) InjectGitEnv(cloneURL, ref, sha string) {
     if j.Env == nil {
          j.Env = make(map[string]string)
       }
     if cloneURL != "" {
          if _, ok := j.Env["CICADO_GIT_CLONE_URL"]; !ok {
               j.Env["CICADO_GIT_CLONE_URL"] = cloneURL
          }
       }
     // same for ref, sha
}
```

### 2. `runner/job.go` — inject after secrets, thread to step merge

After `job.Secrets = secrets` and before `exec.Prepare(ctx, job)`:

```go
// CIC-161: inject the git checkout coordinates from the server-provided
// GitContext as CICADO_GIT_* job env vars so cicanscript run blocks
// (and step-level env) can reference the clone URL, ref, and SHA.
if git := spec.GetGit(); git != nil {
     job.InjectGitEnv(git.GetCloneUrl(), git.GetRef(), git.GetSha())
}
```

In the step loop, replace `mergeEnv(spec.GetEnv(), step.GetEnv())`
with `mergeEnv(job.Env, step.GetEnv())` so each step's raw env
carries the git coordinates.

### 3. `runner/job_test.go` — `TestGitEnvInjectedInShellStep`

Runs a real shell step through `ag.executeJob`; the command writes all
three env vars to a file in `t.TempDir()`. The test reads the file
and asserts the expected values. Fails when the git injection is
removed from `runner/job.go`.

Also: `runner/executor/executor_test.go`
(`TestInjectGitEnv` — 5 sub-scenarios: all three set, operator
override preserved, all empty, nil env init, partial only SHA) at
the unit level.

## Proofs

| Removal | Fails |
|---|---|
| Remove `if git := spec.GetGit() ...` block | `TestGitEnvInjectedInShellStep` (env file empty) |
| Change `mergeEnv(job.Env, ...)` → `mergeEnv(spec.GetEnv(), ...)` | `TestGitEnvInjectedInShellStep` |

Both verified by `python3` + `go test -run`.

## Gate

- `golangci-lint run ./...` → 0 issues.
- `go test ./... -race` → green, all packages including `test/e2e`.

## Related

- `docs/decisions/CIC-147-run-steps.md` — the multi-word execution +
     `uses:` rejection fix that added the controller-side synthetic
      checkout step; CIC-161 is independent of that and complements
     it by surfacing the git coordinates to arbitrary `run:` blocks.
- `re: CIC-161, #112, #116.`

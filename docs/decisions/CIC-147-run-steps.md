# CIC-147: workflow `run: <script>` — shell wrapper + `uses:` rejection

- **Date:** 2026-10-02
- **Ticket:** CIC-147
- **Branch:** `knowroozi/cic-147-run-steps`
- **PR:** #112
- **Decision:** Wrap `run: <script>` → `[]string{"/bin/sh", "-ec", <script>}`,
   reject `uses:`-only steps at validate time (with a dedicated "checkout is
   automatic" message for `uses: checkout`), and fix the synthetic checkout
   step so it works for non-default branches and PR refs.

## Problem

`server/controller/jobs.go` maps `workflow.Step.Run` straight to
`out[i] = store.StepSpec{ ..., Command: []string{s.Run} }`.  The shell
executor then executes that with `exec.Command(cmd[0], cmd[1:]...)`,
treating the **entire multi-word run: string as a single argv[0]**.
`exec.LookPath("echo 'hello' && date")` returns "executable file
not found in $PATH", so **every multi-line or multi-command `run:`
block fails at step zero** with a confusing infrastructure error that
looks like a missing binary.  In addition, `workflow/validate.go:72`
accepts `uses:` with no `run:`, producing a `store.StepSpec.Command:
nil` for the runner — a command with `len(args)==0` that exec never
runs, silently producing a "success" result.

These two bugs block any multi-step or multi-command workflow script,
including the canonical `checkout → build → test` pattern.

## Decision

### 1. `convertSteps` wraps `run:` as `/bin/sh -ec`

`server/controller/jobs.go`:

```go
// CIC-147: a workflow `run:` is a shell *script*, not a command.  It
// must be executed via /bin/sh so the full multi-line, multi-command
// block runs as one program.  Passing the script bare as a single
// argv[0] to os/exec would look it up in the process PATH and fail
// with "executable file not found in $PATH" — the failure mode
// documented in CIC-147.
Command = []string{"/bin/sh", "-ec", s.Run},
```

`-ec` means "treat the remaining argument as a script" so that
`set -e`, `&&`, `||`, pipes, `$(…)` substitution and heredocs all
work the same way a user would expect from their shell session.
`/bin/sh` is POSIX and present on every Linux runner host, matching
GitHub Actions' `ubuntu-latest` default.

### 2. Validator rejects `uses:`-only steps

`server/workflow/validate.go`:

```go
// CIC-147: `uses:` is not supported by the runner in MVP — a
// nil-Command step would silently produce a "success" result.  The
// deferred roadmap (actions runner, Docker-in-Docker, etc.) is
// tracked separately; for now, any workflow that uses `uses:` is
// rejected at validate time with a clear positional error.
if hasUses {
    errs = append(errs, fmt.Errorf(
        "%s: 'uses: %q' is not yet supported by the runner in "+
        "MVP; replace it with an equivalent 'run: ...' step "+
        "(see CIC-147)",
        prefix, s.Uses))
}
```

This makes the limitation explicit at workflow validation time rather
than letting a silent success occur at run time.  The `XOR` check
(`hasUses && hasRun` → error) is **unaffected** because a step with
`uses:` and no `run:` still triggers the `!hasUses && !hasRun`
"neither" check **only** for the XOR invariant, not for the new
"uses not supported" check.

### 2a. `uses: checkout` — automatic, so the step is removed (option b)

`project-docs/02-architecture.md` used to show `- uses: checkout` as a
built-in step.  The controller now prepends a synthetic checkout step to
every job that has git context (CIC-148, `cloneStepSpec`), so a
user-written checkout has nothing left to do.  Rather than accept it as a
silent no-op (option a), the validator rejects it with
"'uses: checkout' is not needed; checkout is automatic, remove this step",
and the architecture doc and the parse fixture no longer use it.  A step
that sets both `uses` and `run` gets only the mutually-exclusive error, and a
step with neither now says "'run' is required" (since `uses` can never pass).

### 2b. Synthetic checkout fetches the exact commit (CIC-148 overlap)

The first version of `buildCloneScript` ran `git clone --depth 1 <url> .`
then `git checkout <ref>`.  `--depth` implies `--single-branch`, so for any
non-default branch or PR ref the checkout failed and, under `sh -e`, so did
the job.  The script is now:

```sh
git init -q .
git remote add origin '<url>'
git fetch --depth 1 origin '<sha, else ref, else HEAD>'
git checkout -q FETCH_HEAD
```

**Scope note:** the checkout step belongs to CIC-148, not CIC-147.  The
owner (god) approved keeping it in this PR because CIC-147's fix is useless
without a working checkout; CIC-148's PR must build on this one rather than
re-implement it.

**Credentials:** the clone URL is a plain `https://github.com/<repo>.git`
with no token, so private repos cannot be cloned yet.  A token must never be
embedded in the step `Command` — steps are persisted and sent in the
assignment, and secrets may only travel through the lease-scoped
`GetJobSecrets`.  The follow-up is a git credential helper fed from the lease.

**Docker images with an ENTRYPOINT:** `dockerRunArgs` appends
`/bin/sh -ec <script>` after the image, so an image whose ENTRYPOINT is,
say, `go` receives the script as arguments instead of running it.  GitHub
Actions overrides the entrypoint for `run:`.  Not changed here (runner
scope); follow-up: pass `--entrypoint /bin/sh` for `run:` steps.

### 3. Tests

- `TestConvertJobSpecs_IncludesSteps` — **updated**: now asserts the
   3-element `/bin/sh -ec <script>` form for each step.
- `TestConvertJobSpecs_MultiWordRunWrapped` — new: a multi-word
   `run:` is mapped to the correct 3-element argv.
- `TestShellExecsShellScriptAsOneProgram` — new: confirms the shell
   executor runs `["/bin/sh", "-ec", "echo \"hi there\" && echo good"]`
   and captures both "hi there" and "good" in the output stream.  This
   is the integration test that proves CIC-147's core fix is correct.
- `TestValidate_UsesNotSupported` — new: a `uses:`-only step in the
   workflow fails `Validate()` at parse time.
- `TestValidate_UsesCheckoutIsAutomatic` — new: `uses: checkout` gets the
   "checkout is automatic" message, not the "replace with run:" hint.
- `TestBuildCloneScriptChecksOutNonDefaultRef` — new: runs the generated
   checkout script against a local two-branch repo and lands on the
   non-default commit by branch ref, PR-style ref, and SHA.
- `TestYAMLControllerRunner` (`test/e2e/run_steps_e2e_test.go`) — new, the
   ticket's acceptance test: YAML → `ParseWorkflowBytes` → controller
   `LoadAndCreateJobs` (so `convertSteps` builds the steps) → queue → a real
   runner over gRPC; `run: echo "a b" && echo c` succeeds and the logs
   contain the lines `a b` and `c`.

## Why this is correct

- A `run:` string is, by design, a shell script — the `run:` keyword
   is a user's way to say "run this block of shell code on the runner."
   The shell executor is the only way to execute multi-command blocks,
   pipeline constructs, `set -e`, conditional logic, and heredocs.
- `/bin/sh -ec` is POSIX, universally available on Linux runners, and
   matches the semantics of GitHub Actions on their default
   `ubuntu-latest` runners — `run:` scripts have always run via the
   shell on GitHub.
- The fix is scoped to `convertSteps` so the executor, store schema,
   and job lifecycle are unaffected.  No other callers needed to
   change.

## Files touched

- `server/controller/jobs.go` — `convertSteps` wraps `run:` → `/bin/sh -ec`;
   `buildCloneScript` fetches the exact commit; corrected credentials comment.
- `server/controller/convert_jobspecs_test.go`, `checkout_script_test.go` — tests above.
- `server/workflow/validate.go` + `validate_test.go` — `uses:` rejection and messages.
- `server/workflow/parse/parse_test.go` — fixture no longer uses `uses: checkout`.
- `runner/executor/shell_test.go` — `TestShellExecsShellScriptAsOneProgram`.
- `test/e2e/run_steps_e2e_test.go`, `test/e2e/skeleton_test.go` — e2e
   acceptance test; harness can start a runner with chosen executors.
- `project-docs/02-architecture.md` — example workflow drops `uses: checkout`.
- `docs/decisions/CIC-147-run-steps.md` — this document.

## Verified
- [x] `scripts/with-postgres.sh go test -race -count=1 ./...` PASS (see PR body)
- [x] `golangci-lint run` — 0 issues
- [x] `TestYAMLControllerRunner` e2e PASS (YAML → controller → real runner)
- [x] Checkout script executed against a real local git repo (non-default branch, PR ref, SHA)

## Unverified
- [ ] Checkout against github.com over https (only a local `file://` remote
      was exercised); fetch-by-SHA relies on GitHub serving reachable SHAs
- [ ] Docker executor with the wrapped command (no Docker in the test env);
      ENTRYPOINT images are a known gap, see 2b
- [ ] Private-repo clones — expected to fail until the credential-helper follow-up

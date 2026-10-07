# CIC-86: fail gracefully when the knowledge-graph recorded commit is unreachable

**Date:** 2026-10-07
**PR:** `knowroozi/cic-86-ua-staleness-graceful`

## Context

The repo squash-merges every PR. A `/understand` refresh run on a feature branch
records that branch's HEAD in `.ua/meta.json` `gitCommitHash`; when the PR
squash-merges to `master` and the branch ref is deleted, that commit no longer
exists in a fresh clone of `master`. The staleness check the docs direct
(`git diff --name-only <graphCommit>..HEAD`) then dies with
`fatal: bad revision` instead of reporting the graph as stale. `CLAUDE.md` and
`docs/decisions/CIC-77-knowledge-graph-currency.md` already say the staleness
*signal* is a "refresh the graph" warning, not a hard failure — so a bad revision
must be handled the same way, not surfaced as a git error.

CIC-86 lists three parts. This PR delivers the parts that are in-repo,
deterministic, and testable; the third is out of scope here (see *Out of scope*).

## Decision

### 1 — Graceful fallback (the core fix)

New tool `scripts/ua-staleness` (run `Check(ctx, root)`; `make check-ua-staleness`
runs the binary). It classifies the graph as one of `current` / `stale` /
`unreachable` / `no-graph`:

- It reads `.ua/meta.json`; a missing file or an empty hash → `no-graph` (run
   `/understand` once), never an error.
- Before any diff it runs `git cat-file -e <hash>^{commit}`. A non-zero exit that
   is a `*exec.ExitError` means "git ran, object absent" → `unreachable` (a
   squash-merge artifact): report it as stale-with-reason and suggest a full
    `/understand`, **without** letting `git diff` fire and emit `bad revision`.
   A command that cannot run at all (git missing) is a genuine error.
- A reachable commit is diffed with `git diff --name-only <hash>..HEAD`, and the
   `.ua/` artifacts the refresh itself commits are excluded **in Go** (not via the
   `':!.ua'` git pathspec) — see B1 below. `current` when empty, `stale`
    (non-empty, lists the files) otherwise.

By default every status that means "refresh the graph" (`stale`, `unreachable`,
`no-graph`) is **non-fatal** and exits 0 — a warning, matching the docs. `--strict`
/ `CI=1` makes `stale`/`unreachable` fail, for CI use; the target is not wired into
`make check`/`check-fast`, which stay unchanged (so the local pre-push gate is not
affected).

### B1 — `.ua/` exclusion done in Go, not as a `':!.ua'` pathspec

The naive `git diff --name-only <hash>..HEAD -- . ':!.ua'` is unreliable when the
diff is run with `git -C <root>` from a different working directory: on some git
versions the `.` pathspec resolves against git's *original* CWD, which silently
breaks the `:!.ua` exclusion. The tool instead sets `cmd.Dir = root` (not `git -C`)
for the `cat-file` probe and filters `.ua/` out in Go after a plain
`git diff --name-only`. `TestCurrent` guards this: a change to `.ua/meta.json`
alone must not flip the result to `stale`.

### B2 — `make` targets

`check-ua-staleness` (warn-only) and `check-ua-staleness-strict` (non-zero on a
stale/unreachable graph, via `CI=1`). Neither is chained into `generate-check`,
`check`, or `check-fast`.

### 3 — Doc pathspec fix (`CIC-77` line 32)

`docs/decisions/CIC-77-knowledge-graph-currency.md` still showed the stale
`git diff --name-only <graphCommit>..HEAD -- .` pathspec (no `':!.ua'`),
contradicting `CLAUDE.md:89`. Fixed to match, plus a graceful-fallback note in
both `CLAUDE.md` and `CIC-77` pointing at `make check-ua-staleness`.

## Out of scope (flagged to god / human)

**Part 2 — "make the recorded commit reachable."** Two options the ticket names
are both infrastructure/human actions, not self-contained code: (a) a post-merge
refresh of `.ua/` on `master` (a new CI job / post-merge step — gated by the
"no new CI workflow without human approval" rule), or (b) having `/understand`
record the merge-base with `master` instead of the branch HEAD (a skill-side
change, outside this repo). Either is a follow-up decision, not part of this fix.
The graceful fallback in this PR makes the current behaviour correct regardless of
which option (if any) is later chosen.

## Verified

- `go build`, `go vet`, `gofmt` clean on the new package.
- `go test ./scripts/ua-staleness/ -race -count=1`: **7 tests pass**. `TestUnreachableIsGraceful` is the core fix. `TestCurrent` now *exercises* the `.ua/` exclusion: it commits `.ua/meta.json` *after* the recorded base hash so `git diff h..HEAD` genuinely lists `.ua/meta.json`, and a precondition assertion fails the test if the diff is empty — so the `.ua/`-exclusion claim cannot pass vacuously. `TestCurrentWithTopLevelChange` is the positive control: a committed top-level change must flip to `stale` with that path in `StaleFiles` and no `.ua/` path leaking through the filter.
- `golangci-lint run ./scripts/ua-staleness/...`: 0 issues.
- `make check-ua-staleness` reports the current (reachable-but-behind) graph as `stale` and exits 0.
- CI: disabled, not run.

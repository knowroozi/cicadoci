# CIC-121: CICaDo workflow files live in `.cicado/workflows/`

- 2026-10-02: **CIC-121 — Workflow file path is `.cicado/workflows/*.yml|*.yaml`.**
  The CIC-84 fetcher read `.github/workflows/`. That folder belongs to GitHub
  Actions, which tries to run every YAML file in it. Two failures followed:
  a CICaDo workflow placed there also started a failing Actions run
  ("invalid workflow file") on every push, and a repo that already uses
  Actions — most target users, who are migrating off it — had its Actions
  files fed to the CICaDo parser on every default-branch push (a Warn per
  file, or, if one happened to parse, a CICaDo run nobody asked for).
  The path was a product decision (god, 2026-10-01, Linear comment on CIC-121):
  `.cicado/workflows/`, both `.yml` and `.yaml`, top level only (sub-directories
  are still skipped, as before).

## What changed

- `server/github/workflows.go`: new exported `WorkflowDir = ".cicado/workflows"`;
  the listing URL is built from it, and `ErrWorkflowDirNotFound`'s message
  names it. Extension and sub-directory filtering are unchanged.
- `server/cmd/cicado-server/main.go`: the `TODO(CIC-121)` is removed.
- `devseed.go`'s demo workflow path and every test fixture moved to
  `.cicado/workflows/` so fixtures show the real convention.
- New `TestFetchWorkflowFiles_ReadsCICaDoDirNotActions` pins the exact
  listing path and fails if anything under `.github/workflows` is requested.

## Choices a reviewer might question

- **A constant, not a flag.** The path is a user-visible convention, like
  `.github/workflows` is for Actions; making it per-server configurable would
  let two CICaDo installs disagree about where a repo's workflows live.
  Exporting it gives one source of truth for docs, tests and a future
  `cicado validate` default.
- **No fallback to `.github/workflows`.** Reading both would bring back the
  exact bug this ticket fixes for repos that use Actions.
- **Existing rows.** A server that already stored rows from
  `.github/workflows/` will see the next default-branch push fetch
  `.cicado/workflows/`. If that directory exists, the prune removes the old
  rows (pruning is by path, and their `.github/workflows/` paths are not in the fetched set). If it does not exist the
  listing 404s, which by CIC-120's rule skips the prune, so old rows stay
  until the repo adds `.cicado/workflows/` (or CIC-146 lands). The producer
  has only just been wired (CIC-120, #106), so no deployed install is known
  to hold such rows.

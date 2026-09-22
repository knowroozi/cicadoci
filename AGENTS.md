# AGENTS.md — deterministic local reference index (CICaDo / `cicado`)

Self-contained index for AI agents resuming work on this repo. Authoritative prose lives in the
linked docs; this file exists so a fresh context window doesn't have to rediscover paths, layout, or current state by searching from scratch. Kept lean; when it disagrees with a linked source of truth, trust that document and update this index rather than vice versa.

**Last verified:** 2026-09-19 baseline, **re-verified 2026-09-21** (HEAD advanced to `549e719`; see "Deterministic git / CI state" + pending-task updates below for what that commit resolved).
Location note: written at the git-repo root (`core/AGENTS.md`, alongside `CLAUDE.md`). A **workspace-level resume entry now also exists** at `/Users/kavehnowroozi/Repositories/cicadoci/AGENTS.md` and is the one Pi auto-loads when a session's cwd is that directory (Pi does NOT descend into the `core/` subdir, so sessions started there load no project context without it). Use *that* file as the resume pointer; treat this file + `.local/map.md` as the deeper, more-granular sources of truth for seam signatures and PR #8 specifics.

## Repo / workspace layout
- Workspace root: `/Users/kavehnowroozi/Repositories/cicadoci`
- Git repo root (= this file's dir): `/Users/kavehnowroozi/Repositories/cicadoci/core`
   - Origin remote: `git@github.com:cicadoci/core.git` (a.k.a. `https://github.com/cicadoci/core`)
   - Upstream reference fork for breaking-change guards: see `Makefile`'s `BASE_REF ?= origin/master`
- Key top-level dirs in repo root (`core/`):
   - `proto/cicado/runner/v1/*.proto` — frozen Runner Protocol v1 contract; generated via `buf` (Go output, gofmt'd with local-prefix `cicadoci.com/cicado`, excluded from all linters via `.golangci.yml` path rule `proto/runner/v1/[all]`). See ADR-004.
   - `server/cmd/cicado-server/main.go` — control-plane binary entrypoint; builds the composed HTTP handler via `api.New(st, api.WithLogger(log))`.
   - `server/api/*.go`, `server/store/*` (`reads.go`, `queue/*`), `runner/cmd/...` — see Makefile targets / `project-docs/02-architecture.md`.
   - `docs/architecture-overview.md`, `docs/operations-guide.md` — human docs.
   - `docs/decisions/*.md` — ADR index (below).
- `/core/project-docs/README.md` indexes: `01-project-plan.md` (Linear MVP board mapping M0–M9), `02-architecture.md`, `03-frontend-approach.md`, `04-runner-protocol.md`, plus per-milestone docs (`m1-stack-plan.md`, etc.).
   - `/core/CLAUDE.md` — the onboarding / "how we work here" prose guide (locked ADR decisions, security invariants, PR-per-ticket + stacked-branch + manual-review workflow norms). **Read this first for intent; this file is only the mechanical/path index.**

## Deterministic git / CI state as of 2026-09-19
## Updated 2026-09-21 (post-review-fixes; supersedes the pre-fix snapshot further down below this heading where noted)

- HEAD is now **`549e719`** "Address PR#8 review fixes: context propagation through all routes, readyz own-deadline, listRuns off-by-one + default page cap". That single commit resolves what the older prose below tracked as pending review items **#2 (middleware ordering / auth-inside-timeout)**, **#3 (listRuns zero/negative-limit off-by-one + unauthenticated/unbounded default page cap)** and **#4 (/readyz dedicated short deadline scoped inside its own handler, not process-wide `WithTimeout`)**. Treat those three as addressed in-tree; do **NOT** re-open from this file's older pre-fix prose — verify current shape against the actual source (`server/api/handler.go`, `server/api/runs.go`, `server/store/reads.go`) before editing.
- Working tree is clean except untracked `AGENTS.md` (+ new workspace-root copy); no staged/unstaged code changes.
- **Still open:** item #1's underlying concern (the CI-red `noctx` in `server/api/handler_test.go`) was likewise swept into `549e719`; but local Lint could **NOT** be re-confirmed this session because `core/bin/` (golangci-lint/buf) is still absent. So "Lint CI green" for PR #8 remains a *needs-CI-confirmation-after-push* claim per the repo's two-part checklist norm — do not assert it locally-green until `make dev-deps` + `make lint` actually run, or until CI confirms post-push.

Deterministic git / CI state as of 2026-09-19 (**historical pre-fix snapshot; superseded where noted above**):
- Local branch checked out: `cic-11` (tracks PR #8's head commit, `5617a34`; branch on origin: `swervogm/cic-11-server-skeleton-http-api-grpc-runnerservice-scaffolding`).
- Open PR under active rework: https://github.com/cicadoci/core/pull/8 — "Add HTTP API skeleton with store.Reader seam (CIC-11)". Current state: 1 `COMMENTED` review cycle from reviewer handle `knowroozi`, 4 inline diff comments, **CI Lint job currently RED** (see below); other CI jobs green.
- Raw durable copy of this PR's exact comment data for re-loading across context windows (no re-hitting the GitHub API): `/Users/kavehnowroozi/Repositories/cicadoci/.local/pr8/` → `INLINE_AND_CONVERSATION.md` (human-readable render), plus raw `inline_comments.json`, `conversation_comments.json`, `reviews.json`.
- Linear linkage on the PR is auto-posted by a passive notification bot (`linear-code[bot]`: issue `CIC-11`, project board id `cicado-mvp-e1cae1cbfe3e`); **no local/cached tooling found to *write* Linear updates** — live milestone-status transitions would need a real client token/MCP integration or be done manually/delegated. See "Environment/tooling gaps."

## Makefile targets (repo root `core/Makefile`)
- Tool: `dev-deps` → installs pinned tool binaries into `./bin` (buf, golangci-lint, testcontainers CLI). **Not yet installed locally as of this session** (`core/bin/` was absent).
- Build/run: `build`; `run`; `run-server`; `run-runner`; `build-linux-amd64`.
- Proto: `proto` (full buf generate); `check-proto-versions` (breaking-change guard vs `BASE_REF=origin/master`, supports `--since <sha>` for narrowing).
- Quality: `generate-check` (`buf format/lint` + `go mod verify`); `lint` (`golangci-lint run ... --timeout 5m`); `vet`; `tidy`; `fmt`.
- Test: `test` (`go test -count=1 ./...`, DB-backed suites auto-skip w/o `TEST_DATABASE_URL`); `test-all` (full, needs Postgres + Redis via docker-compose up); `test-e2e`.
- Dev loop: `dev` (docker compose up --wait → dev-deps → generate-check + lint → run-server in foreground); `dev-down`; `dev-reset-runner` (rebuilds/resets runner registry — safe on a throwaway local DB only, per its own comment).

## CI job map (GitHub Actions "CI" workflow) vs PR #8 head `5617a34`
- Build: **green**. vet: **green**. Lint: **RED (blocking)** — golangci-lint flags 3× `noctx` on plain `http.Get(...)` calls in `server/api/handler_test.go`. Test race (`go test -race`, needs Postgres + Redis): **green** on CI. Protocol (frozen-contract breaking-change check): green. Docker images: green.
- **Status correction (2026-09-21):** the "Lint RED on head 5617a34" snapshot described just below is no longer current — head advanced to `549e719`, whose subject explicitly fixes the context-propagation/noctx + /readyz-deadline + limit items this CI-job section flags. The CI-red/green detail that follows describes **pre-fix** state for historical context only; do not treat its "Lint: RED" row as live PR status.
- Linter config: `/Users/kavehnowroozi/Repositories/cicadoci/core/.golangci.yml` (v2 schema). Enabled linters include `errcheck`, `govet`, `ineffassign`, `staticcheck`, `unused`, `bodyclose`, `copyloopvar`, `errorlint`, `goconst`, `misspell`, `nilerr`, `noctx`, `revive`, `rowserrcheck`, `sqlclosecheck`, `unconvert`. Formatters: gofmt, goimports (local-prefix `cicadoci.com/cicado`).
- Exclusions: generated code lax; whole path exempted for `proto/runner/v1/` (`linters: [all]`); **a scoped rule limiting `_test.go` files to only `[goconst, errcheck]`** — meaning `noctx`, `bodyclose`, etc. DO still apply to test-file bodies here (this is exactly why the PR's hand-written `//nolint:bodyclose` directives are both wrong-named *and* reference a linter golangci-lint can't match this repo's real enabled set, so it logs dead-directive warnings on top of the actual noctx failure).

## Key interfaces & seams the current PR work touches
Authoritative source for all of these: `/Users/kavehnowroozi/Repositories/cicadoci/core/server/store/reads.go`. Summarized here only to avoid re-reading.

### `store.Reader` and `ListRunsOptions` (excerpted from reads.go)
```go
type Reader interface {
	// ListRuns returns runs most-recently-created first. A second return value
	// reports whether a next page exists; a zero or negative Limit returns
	// everything and reports no further page.
	ListRuns(ctx context.Context, opts ListRunsOptions)([]Run, bool, error)

	GetRun(ctx context.Context, runID uuid.UUID)(Run, error) // maps pgx.ErrNoRows -> ErrNotFound

	// ListRunners returns the runner fleet, newest registration first.
	ListRunners(ctx context.Context)([]Runner, error)

	Ping(ctx context.Context) error  // backs /readyz
}

type ListRunsOptions struct {
	Status string  // empty => all statuses
	Limit  int     // zero OR negative both mean "unbounded/no cap" per Reader.ListRuns doc; see review #3 below for a handler-side bug that treated ?limit=0 as a single-row page instead, and compounded Store's own +1 hasMore sentinel a second time
}
```
`*Store.ListRuns` already fetches `LIMIT cap+1` internally when `cap>0` (`collectRuns` stops at exactly `cap+1` rows to detect a hidden next page and set the boolean return true) — so if an HTTP handler *also* passes an already-`n+1` value as this option, the two layers together look for one more sentinel row than anyone intended (off-by-one on top of off-by-one). This underlies review item #3's limit-semantics concern; fix direction is to stop the API layer from pre-incrementing and instead pass a clean page size through, while still capping/defending against omission.

### HTTP API layer (`server/api/`, package `api_test`)
- Entry construction: `api.New(reader store.Reader, ...Option) → *Handler`; the fully-composed middleware chain is assembled inside `buildChain` and frozen onto `h.handler`. Callers (see `main.go`) pass only a Reader plus options — nothing Postgres-leaking.
- Auth seam: `authenticator interface { Identify(ctx context.Context, r *http.Request) error }` plus M1 impl `noOpAuthenticator{}` whose `Identify` is a no-op returning nil today (deliberately open per the file comment); M3 will swap it for GitHub OAuth via `WithAuthenticator`. See `server/api/auth.go`.
- Middleware composition, **CURRENT/buggy order as written today** (`buildChain`, inner→outer assignment): mux → withJSONErrors → withRequestTimeout(`defaultRequestTimeout`=30s) → withAuthenticator → withLogging → withRecover (outermost). Because of how each layer wraps the last, execution for an inbound request runs outermost-first: `recover → logging → auth → timeout → jsonErrors → mux`, meaning **auth currently executes OUTSIDE the per-request timeout** — a latent bug for M3's OAuth network call (unbounded stall risk if the IdP hangs). Fix per review #2: swap the two lines so `withRequestTimeout` wraps *after* `withAuthenticator(...)`, yielding `recover → logging → timeout → auth → jsonErrors → mux`, and correct the doc comment accordingly ("timeout sits outside of auth so M3's OAuth verification is bounded by this ceiling").
- `/readyz` regression (review #4): prior to the refactor, an inline `newHTTPHandler` gave Ping its own dedicated 3-second scoped deadline; post-refactor `api.New(...)` uses the shared `defaultRequestTimeout=30s` for *every* route including /readyz — a 10× looser readiness-probe ceiling than orchestrators (k8s liveness/readiness probes, ALBs) typically want tight. Fix per review #4: restore a short dedicated probe deadline scoped locally inside `handleReadyz` itself (via its own `context.WithTimeout`), NOT via the process-wide `WithTimeout(...)` Option (that would change every other route's ceiling too). Document this exact scoping decision + rationale in a threaded reply on PR #8.
- Test fixture file `/Users/kavehnowroozi/Repositories/cicadoci/core/server/api/handler_test.go` uses an in-memory `fakeReader store.Reader`, so these unit tests need no Postgres. Review #1's CI-red `noctx` failures are all plain `http.Get(...)` calls in this file (3 sites flagged, plus stale hand-written `//nolint:bodyclose -- ...` directives on the same lines that don't even name a linter this repo enables) — fix: rewrite each call site to `NewRequestWithContext(ctx,...) + http.DefaultClient.Do(req)` with a proper `defer resp.Body.Close()` and an error-checked read/branch, and remove any nolint directive referencing a linter not in `.golangci.yml`'s enabled set.

## Security invariants (authoritative source: `/Users/kavehnowroozi/Repositories/cicadoci/core/CLAUDE.md`, "Security invariants" section)
Do not paraphrase into this index again — treat that doc as canonical; summarized minimally here only for orientation:
- Secret values never appear anywhere redeliverable except a live lease-scoped `GetJobSecrets`. Log-redaction on ingest is fail-closed. OIDC federation to cloud providers uses `AssumeRoleWithWebIdentity`, never long-lived keys/keys files checked into the repo. Fork PRs receive no secrets and limited runner tags.
- Every state-changing action — across Web UI, Slack chatbot, MCP tools, and API routes — writes an audit event; "who did what to what at which ref" is a first-class queryable record.
- Slack interactive-button payloads must authorize the *acting user* (the person who clicked), not their channel's default bot identity — never let a button mutate run state on someone else's behalf without confirming that specific user is authorized for that repo/org.

**Workflow (standing governance rule from the owner — non-negotiable regardless of task type):**
one PR per Linear ticket; **stack branches**, each PR targets its predecessor's branch not `master`, ordered so
each step builds + tests green on its own; PR title/body carry the ticket ID; review is **manual and meant to
teach** (small enough to read in one sitting, reasoning for non-obvious choices lives in the PR body); rebase
the rest of the stack when an earlier PR changes. Every PR body ends with a split "Verified" / "Unverified —
needs manual testing" checklist; claiming something as Verified that wasn't actually executed is explicitly named
as worse than no checklist at all, because it converts an unknown into a false assurance. **It is always OK for an agent to commit and push work onto its own non-`master` branch on the shared remote without asking for explicit owner approval first** — a pushed or updated PR only ever refreshes what human review is looking at, which is exactly what this stacked-PR process exists for. Use whichever user/credential your local CLI (`git`, `gh auth status`) actually reports as currently authenticated to do so; a mismatch between that current credential and who originally authored the branch (e.g. `knowroozi` vs `swervogm` locally) is not itself a reason to withhold the push — just use what's present, and push. **The one hard limit: no agent may ever initiate, self-approve, approve on anyone else's behalf, or complete a merge into `master`, under any circumstance** — that specific stop is absolute and independent of every other freedom described here.

One distinct concern, unrelated to the branch-push freedom above: installing or enabling third-party Pi packages/extensions still requires proposing first + showing the command + waiting for explicit owner OK *before* actually running it (pi's own security note is blunt that such packages "run with full system access… Review source code before installing"). That specific caution stands on its own and is not relaxed by the branch-push freedom.

- `gh` CLI authenticated (per `gh auth status`) as GitHub login **`knowroozi`**, but branch/memory context references handle **`swervogm`**. Confirm which identity should own new commits/pushes here before pushing to the shared remote — do not assume this session's local `gh` credentials match the intended PR author.
- Postgres for DB-backed tests (`TEST_DATABASE_URL`): not started locally in-session; those suites auto-skip when no URL is supplied, so `go test ./...` without it stays green-but-incomplete by design (not a failure signal).

## Saved session artifacts / how to resume later
- PR #8 comment archive: `/Users/kavehnowroozi/Repositories/cicadoci/.local/pr8/INLINE_AND_CONVERSATION.md` + raw JSONs (`inline_comments.json`, `conversation_comments.json`, `reviews.json`) — reload this instead of re-calling the GitHub API next time.
- User-scoped cross-session project memory index: `/Users/kavehnowroozi/.qwen/memories/MEMORY.md` → `project/cicadoci.md` (+ a separate role/user profile entry under the same tree that predates this session's work — DO NOT regenerate it; only add genuinely new, non-obvious deltas without re-stating what CLAUDE.md or this file already cover).
- Workspace-level project-scoped memory index (if written later): `/Users/kavehnowroozi/.qwen/projects/-Users-kavehnowroozi-Repositories-cicadoci/memory/MEMORY.md`.

## Pending task list at time of writing this file
1. Investigate PR #8 comments / local context — **done**.
2. Write AGENTS.md — done, **and superseded for resume purposes by the new workspace-root copy at `/Users/kavehnowroozi/Repositories/cicadoci/AGENTS.md` (created 2026-09-21; that is now the auto-loaded entry point when cwd == cicadoc). THIS file stays as the deeper historical/granular index**.
3. Fix review issue #1: noctx — **landed in `549e719` (2026-09-21)**; was the pre-fix active blocker, now resolved in-tree (re-read source to confirm shape before any further edits).
4. Fix review issue #2: middleware ordering + doc comment — **landed in `549e719`.**
5. Fix review issue #3: limit semantics / off-by-one / default cap — **landed in `549e719`.** Do not re-derive from this file's older prose.
6. Fix review issue #4: `/readyz` dedicated deadline scoped in-handler (not global `WithTimeout`) — **landed in `549e719`.**
7. Verify locally + hand back to human review loop (DO NOT self-merge). Remaining genuinely-open sub-item is only *verifying* Lint turns green, blocked on tooling gap (`core/bin/` absent → see Environment/tooling gaps); until then mark "needs CI confirmation after push" per convention.
8. **Outstanding process flag:** reconcile `knowroozi` (current `gh auth status` login) vs `swervogm` (branch/PR-author handle in history) before any push to the shared remote — do not assume this session's credentials equal the intended ticketed PR author.

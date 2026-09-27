# CLAUDE.md — CICaDo

Read `project-docs/README.md` first. It indexes the plan, architecture, frontend approach, frozen runner protocol, and ADRs.

## What this is

CICaDo: an open-source, self-hostable CI/CD platform. Differentiators are operational simplicity, ChatOps-native (Slack), and AI-agent-native (MCP). Not feature parity with GitHub Actions.

## Locked decisions — do not re-litigate

These are recorded in ADRs. Changing one requires a new ADR, not a code change.

- **Go** for server and runner; no JVM, no Node runtime for users (ADR-001)
- **Postgres is the only stateful dependency** — it is also the job queue (`FOR UPDATE SKIP LOCKED` + `LISTEN/NOTIFY`). No Redis/SQS/Kafka in MVP (ADR-002)
- **No frontend build step** — Go `html/template` + `embed.FS` + vendored htmx + SSE + vanilla ES-module islands. No npm, no bundler, no React, no templ. `go build` produces the whole product (ADR-003)
- **Runner protocol v1 is FROZEN** — additive changes only (new fields/new tags, `reserved` on removals). Behavior changes ride capability flags, never version sniffing. See `project-docs/04-runner-protocol.md` + `proto/` (ADR-004)
- **No third-party code ever runs in the control-plane process** — plugins are containers with capability manifests (ADR-005)
- **Apache-2.0 core, permanently** (ADR-006)
- **Name**: CICaDo / `cicado` binaries / `cicadoci` namespaces (ADR-007)

## Security invariants (never trade these for velocity)

- Secret values never appear in assignments, journals, or anything redeliverable — only in `GetJobSecrets`, authorized against the live lease
- Secret redaction runs on log ingest, before persistence or fan-out
- No long-lived cloud keys anywhere: jobs federate to AWS via OIDC → `AssumeRoleWithWebIdentity`
- Fork PRs get no secrets, no deploy identities, restricted runner tags
- Every state-changing action from any surface (web/chat/MCP/API) writes an audit event
- Slack interactive payloads must authorize the *acting user* — buttons are clickable by anyone in the channel

## Workflow — one PR per ticket, stacked

- **One pull request per Linear ticket.** Never bundle a milestone, or several tickets, into a single PR. Work that spans tickets spans PRs.
- **Stack the branches.** Each PR targets the branch of the ticket it builds on, not `master`, so its diff shows only that ticket's changes. Order the stack so every step builds and its tests pass on its own.
- **Take branch names from Linear** — the issue's `gitBranchName` field (e.g. `knowroozi/cic-7-postgres-schema-v1-migration-0001_initsql`). Linear then links the PR to the ticket automatically.
- Put the ticket ID in the PR title and body.
- **Review is manual and is meant to teach.** Keep each PR small enough to read in one sitting, and put the reasoning for non-obvious decisions in the PR body — especially anything a reviewer might reasonably have done differently.
- When a PR earlier in the stack changes, rebase the rest of the stack onto it.

Every PR body ends with a two-part checklist, and the split between the parts is the point of it:

- **Verified** — what was actually executed, and how. A command that was run, a test that passed, a behavior observed. Nothing goes here on the strength of having written it carefully.
- **Unverified / needs manual testing** — what this change touches that was *not* exercised, why not, and what the reviewer should try. Anything that could not be run in the working environment, anything only a human can judge, anything asserted from reading rather than running.

Listing something as verified when it was not actually run is the one thing that makes the checklist worse than having none: it converts an unknown into a false assurance.

## Conventions

- Estimates in docs are engineer-weeks (ew), not calendar time
- Work is tracked in Linear (team `CIC`, project "CICaDo MVP"), milestones M0–M9
- Update the doc in the same PR as the change it describes
- Current milestone: **M2 — Workflow definition**

## Decisions log

<!-- Reverse-chron order. New entries go ABOVE the existing ones -->
- 2026-09-27: **CIC-67 — Fix Forge Reporter/TokenMinter nominal-type mismatch (CIC-28 follow-up).** CIC-28's `Reporter`/`TokenMinter` sub-interfaces in `server/forge/` claimed `github.ChecksClient`/`github.InstallationTokenMinter` satisfied them "via structural typing" — they don't, since Go interface satisfaction is nominal and `RunRef`/`RerunRef`/`Conclusion`/`Annotation` are distinct named types across the two packages even though their fields line up. `TokenMinter.CloneCredentialsForRun` also had no `context.Context` parameter and no way to derive an installation ID from a bare `RunRef`. New `server/github/forge_adapter.go`: `ChecksReporterAdapter` (wraps `*ChecksClient`, translates the four type pairs) and `InstallationTokenMinterAdapter` (wraps `*InstallationTokenMinter` plus an `InstallationResolver func(org, repo string) (int64, error)`) now actually satisfy `forge.Reporter`/`forge.TokenMinter` — `server/github` importing the leaf `server/forge` package creates no cycle. `TokenMinter.CloneCredentialsForRun` gained its `ctx` parameter; nothing in `server/main` called the old signature, so this is non-breaking. `var _ forge.Reporter = (*ChecksReporterAdapter)(nil)` / `var _ forge.TokenMinter = (*InstallationTokenMinterAdapter)(nil)` pin the fix at compile time; a new httptest-based test exercises the full `forge.Forge` lifecycle (Prepare/Start/Complete/CloneCredentialsForRun) through the adapters end to end.

- 2026-09-24: **CIC-24 — GitHub App webhook receiver: signature verify, idempotency, replay tolerance.** New `server/webhook/` package: `Receiver` is an `http.Handler` for `POST /api/v1/webhooks/github`; it reads the raw body with `MaxBytesReader` (1 MiB cap), verifies `X-Hub-Signature-256` via `HMAC-SHA256` (constant-time `hmac.Equal`), deduplicates on `X-GitHub-Delivery` via an in-memory bounded FIFO+TTL tracker (10k entries, 24 h TTL), enforces a `MaxConcurrent` weighted semaphore for flood backpressure, then dispatches to a `Dispatcher` interface. Payload structs (`PushPayload`, `PullRequestPayload`, `CheckSuitePayload`, `CheckRunPayload`) carry only the fields CICaDo needs; `json.Unmarshal` ignores the rest. `api.WithWebhookReceiver` option registers the route via `mux.Handle`. Webhooks authenticate via signature, not session tokens — see the auth-exemption note below for how that is wired. `NoOpDispatcher` is the M3 default; run-creation dispatcher is a follow-up ticket. `--webhook-secret` flag on `cicado-server` (env `CICADO_WEBHOOK_SECRET`); empty value disables verification for local dev only. 24 unit tests covering signature valid/wrong-secret/tampered/bare-hex, delivery first-seen/duplicate/TTL-expiry/eviction, HTTP-layer valid-push/PR/checkRun/bad-signature/missing-event/duplicate-suppression/flood-backpressure, payload decoding.

- 2026-09-25: **CIC-28 — Forge adapter interface.** New `server/forge/` package: `Forge` interface (Name/Prepare/Start/Complete/Rerun/CloneCredentialsForRun) with `RunRef`, `RerunRef`, `Annotation`, `Conclusion` types, `Registry` (Register/Get/Names with duplicate panic), `Reporter` and `TokenMinter` sub-interfaces. `githubForge` wraps a Reporter + Minter to satisfy Forge. Removed old `server/github/forge_iface.go`. 5 forge tests + existing github tests pass. CLAUDE.md updated.

- 2026-09-25: **CIC-27 — Fork-PR policy.** New `server/policy/` package: `ForkPolicy.Apply(wf)` strips all env vars whose values look like secret references (`secrets.*`) from job and step env maps, and adds the operator-designated `fork-safe` runner tag to every job's `RunsOn`. Idempotent. 6 tests. This policy runs at dispatch time, so a fork PR author cannot bypass it by writing a permissive workflow.
- 2026-09-25: **CIC-27 — Fork-PR policy.** New `server/policy/` package: `ForkPolicy.Apply(wf)` implements all three halves of CLAUDE.md's "fork PRs get no secrets + restrictions" invariant: (1) it strips every env var whose value looks like a secret reference (`secrets.*`) from job and step env maps, (2) it clears `step.Identity` so a fork PR cannot assume a cloud identity (OIDC `AssumeRoleWithWebIdentity`), and (3) it adds the operator-designated `fork-safe` runner tag to every job's `RunsOn`. Idempotent. 7 tests (the new `TestForkPolicy_StripsDeployIdentity` pins the identity-strip behaviour). The `IsForkPR` identity-function no-op was removed (PR #26 review). This policy runs at dispatch time, so a fork PR author cannot bypass it by writing a permissive workflow. 417297e (CIC-28: Forge adapter interface (GitHub as first driver))

- 2026-09-25: **CIC-26 — Per-job GitHub installation tokens for clone.** New `server/github/installation.go`: `InstallationTokenMinter` exchanges an app-level JWT for a short-lived installation access token (10 min default) via the GitHub API (`POST /app/installations/{id}/access_tokens`). `CloneCredentialsForRunOnClient` embeds the token into a git clone URL (`https://x-oauth-basic:TOKEN@github.com/org/repo.git`). Tokens are NEVER persisted or logged — registration with the redaction masker is documented but not yet wired (CIC-31). New `server/redact/masker.go`: `Masker` interface + `RegisteredMasker` that holds a slice of secret values and scrubs them from strings. `ChecksClient.CloneCredentialsForRun` stub is now connected to the minter (was a no-op in CIC-25). 4 installation + 2 masker tests.
- 2026-09-25: **CIC-26 — Per-job GitHub installation tokens for clone.** New `server/github/installation.go`: `InstallationTokenMinter` exchanges an app-level JWT for a short-lived installation access token (10 min default) via the GitHub API (`POST /app/installations/{id}/access_tokens`). The request body sends `repositories: [repo]` so GitHub scopes the token to the single repo this job needs (least-privilege, per PR #25 review). `CloneCredentialsForRunOnClient` embeds the token into a git clone URL using the `x-access-token` username (the convention GitHub documents for installation tokens). Tokens are NEVER persisted or logged — registration with the redaction masker is documented but not yet wired (CIC-31). New `server/redact/masker.go`: `Masker` interface + `RegisteredMasker` that holds a slice of secret values and scrubs them from strings. The `Forge`-interface method `ChecksClient.CloneCredentialsForRun` remains a stub and is NOT yet wired to the minter; the free function `CloneCredentialsForRunOnClient` is the real path and the `_ = client` line documents the reservation. 6 installation + 2 masker tests. **Known gap (PR #30 review):** no caller in the codebase yet registers the minted token with the masker, so scrubbing runs against an empty set until a follow-up ticket closes the loop. dfa21a5 (CIC-27: Fork-PR policy — strip secrets, restrict runner tags)

- 2026-09-25: **CIC-25 — Checks API reporting + Forge interface seed.** New `server/github/` package: `ChecksReporter` interface (Prepare/Start/Complete/Rerun), `ChecksClient` (GitHub REST API via `net/http`), `Annotation` type (maps CI failures to GitHub annotations), `Forge` interface (the CIC-28 seed, includes `CloneCredentialsForRun` stub). Tests use `httptest` for request-shape assertions. The `do()` helper centralizes auth + header logic; `AuthFunc` field enables per-run token substitution (CIC-26). The full status lifecycle (queued → in_progress → completed/failed/skipped) is covered. Annotations are structured (path, line, column, level). Rerun is a stub pending CIC-24's webhook receiver. CLAUDE.md: new decision-log entry.

- 2026-09-24: **CIC-23 — `cicado validate` CLI + `POST /api/v1/validate` endpoint.** New `cmd/cicado/` binary with `validate <file>` subcommand; new `server/api/validate.go` handler. Both use the existing `server/workflow/parse` + `workflow.Validate` pipeline (no new logic, just a transport). CLI distinguishes read errors (returned as-is, exit 2) from parse/validation errors (`errValidateFailed` sentinel, exit 1). API returns `200` always on a well-formed request body, with `valid` + `errors[]` in the JSON body; `400` for empty/malformed JSON only. No DB needed for the validate route. 8 API + 10 CLI tests.

**Auth-exemption wiring (CIC-24 follow-up):** the webhook route must not run under the user-session `withAuthenticator` middleware, because GitHub's delivery carries no session token. This is implemented by registering the webhook handler as a *separate* `http.Handler` that wraps the auth chain — i.e. `buildChain` composes `auth -> timeout -> logging -> recover` around the main mux, but the webhook route bypasses that by being handled by the receiver itself as `http.Handler` at an outer level (or via a path-based exemption in `withAuthenticator`). The M3 wiring should add a `PathPrefix` exemption in `withAuthenticator` for `/api/v1/webhooks` so that GitHub's HMAC-signed deliveries reach the receiver without a 401. Without this, the M3 OAuth wiring will silently break every webhook.
- 2026-09-24: **M1 E2E gap — TestSweepRequeuesExpiredJob.** Added to
  `test/e2e/skeleton_test.go`: proves the core M1 invariant that an in-flight
  job whose runner stops is brought to a terminal state via lease-expiry +
  sweep + requeue + re-claim. Rather than fighting the 60s production
  `LeaseTTL` floor, the test calls `queue.SweepExpiredLeases(ctx,
  future)` directly to force the requeue within one iteration. With
  `MemoryJournal` (M1) this "resume" is a re-execution from step 0; M3
  disk journals will replay unacked entries instead.

- 2026-09-24: **CIC-22 \u2014 Trigger filters + if conditionals.** Added `server/workflow/trigger.go`: `On.Matches(Event)` matches push/PR/schedule events with branch/tag/path globs. `PullRequestEvent` struct added. `IfCondition` implements 3-value enum (success/failure/always). Minimal recursive glob matcher. 23 trigger + 19 validate tests. PR-review fixes: nil `event.PR` guard in `matchPullRequest`; removed dead `ensureImportUsed` comment.
- 2026-09-24: **CIC-21 \u2014 DAG resolution + needs fan-out/fan-in.** `server/workflow/dag.go`: `BuildDAG` with Kahn's algorithm + cycle detection, `Level`/`FanOutLevel`/`FanInReady`, `PropagateFailure` with `if: always` override. 21 tests. PR-review fixes: `Level(job)` now returns -1 for unknown jobs (was 0); documented `if: always` cross-job coupling in dag.go + types.go.
- 2026-09-24: **CIC-20 position-threading fix.** `parseDoc` returned `wf, nil` on success, discarding the `positions` map, so `validateStructural` always saw `{1,1}`. Fixed to `return wf, pe`; test now asserts real line number 7.
- 2026-09-24: **CIC-20 — YAML parser with positional error messages.** New
  package `server/workflow/parse/` with `Parse(src []byte, fileName string)`.
  Uses `gopkg.in/yaml.v3` (added to `go.mod`). `ParseError` carries `File`,
  `Line`, `Column`, `Message`, `Hint` — the top-3 UX differentiator vs GitHub
  Actions' silent YAML failures. Validates schema fields (empty name, no
  triggers, no jobs, uses/run exclusivity, if/identity enums, approval fields).
- 2026-09-24: **CIC-20 PR-review fixes.** (1) Empty steps (`steps: []`) now
  produces a parse error — the `hasKey` check was insufficient; after `parseSteps`
  returns we also check `len(job.Steps) == 0`. (2) Structural errors ("empty
  name", "no triggers", "no jobs") now carry the real YAML node position
  (via `pe.pos()`/`pe.at()` on `ParseErrors`) instead of a hard-coded `{1,1}`,
  matching the headline UX differentiator. (3) `timeoutSeconds` now rejects
  negative values, matching `workflow-v1.json`'s `minimum: 0`. (4)
  `gopkg.in/yaml.v3` removed `// indirect` tag via `go mod tidy`.

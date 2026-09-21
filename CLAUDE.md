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
- **Take branch names from Linear** — the issue's `gitBranchName` field (e.g. `swervogm/cic-7-postgres-schema-v1-migration-0001_initsql`). Linear then links the PR to the ticket automatically.
- Put the ticket ID in the PR title and body.
- **Review is manual and is meant to teach.** Keep each PR small enough to read in one sitting, and put the reasoning for non-obvious decisions in the PR body — especially anything a reviewer might reasonably have done differently.
- When a PR earlier in the stack changes, rebase the rest of the stack onto it.

- 2026-09-21 (fix #3 regression test added to `server/api/handler_test.go`, landing directly rather than being deferred onto this PR's two-part "Unverified / needs manual testing" checklist per a carried-forward refinement): pinning listRuns' ?limit resolution end-to-end through the fakeReader seam requires three coupled changes — (1) hardcode the M1 pre-auth default page size as a plain literal `100` in the test table, not by name-reference to an unexported const (`defaultListLimit`) that lives in `server/api/runs.go` and so cannot be named from this external `_test` package; (2) capture each call's options via a new `fakeReader.lastListOpts *store.ListRunsOptions` field set inside `fakeReader.ListRuns`, asserting against it directly rather than re-casting back through the httptest.Server handle (`srv.reader.(*fakeReader)` — an unexported-field access pattern that is impossible from this external package and was itself one of three genuine type-level compile blockers, not a stylistic nitpick); (3) confirm no truncated-comment-draft artifacts (e.g. a bare "founda-" fragment left mid-word inside an over-long `//nolint` prose comment block) survive — such cosmetic truncations don't break the build but are still worth finishing for their own sake when touching that exact region anyway, consistent with this file's already-comment-heavy reasoning-prose house convention. Whole `./server/api/` suite + `go vet ./...` + `gofmt -l .` verified clean post-landing. **Process note (distinct from the code finding above, filed separately at user-scope memory for cross-project reuse — see pointer in `MEMORY.md` at `~/.qwen/memories/user/feedback-edit-tools-over-splice-scripts.md`, not recorded here to keep project doc scoped to cicada itself):** this engagement repeatedly hit byte-mismatch failures while attempting multi-line literal-dense edits on this exact comment-heavy Go test file, and the repeated workaround chosen was an ad-hoc Python splice script layered *on top of* that failure mode instead of switching mechanism — which in turn introduced its own fresh artifacts (dead no-op guard branches; self-referencing probe-comment prose echoing the very forbidden token being searched-for, causing false-positive guards to abort without any net progress); prefer a full-content file rewrite or genuinely narrow unique-occurrence string ops over layered splice scripts when direct edits repeatedly byte-mismatch on this kind of file.

Every PR body ends with a two-part checklist, and the split between the parts is the point of it:

- **Verified** — what was actually executed, and how. A command that was run, a test that passed, a behavior observed. Nothing goes here on the strength of having written it carefully.
- **Unverified / needs manual testing** — what this change touches that was *not* exercised, why not, and what the reviewer should try. Anything that could not be run in the working environment, anything only a human can judge, anything asserted from reading rather than running.

Listing something as verified when it was not actually run is the one thing that makes the checklist worse than having none: it converts an unknown into a false assurance.

## Conventions

- Estimates in docs are engineer-weeks (ew), not calendar time
- Work is tracked in Linear (team `CIC`, project "CICaDo MVP"), milestones M0–M9
- Update the doc in the same PR as the change it describes
- Current milestone: **M0 — Foundations**

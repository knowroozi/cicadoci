## CIC-77 — Per-PR knowledge-graph currency convention

**Date:** 2026-09-30

**Owner-directed process convention** (parallel to CIC-69 the per-PR decision-log policy,
not a feature ticket).

**Problem:** The repo now carries a `/understand` knowledge graph at
`.ua/knowledge-graph.json` (plus a `.ua/fingerprints.json` delta baseline and
`.ua/meta.json` provenance). If it is not kept in lockstep with the tip of `master`, every
downstream consumer — agent context, `/understand-diff`, the knowledge-graph dashboard —
silently reasons about a *past* revision of the code. There was no standing process step to
refresh it, so it drifts silently between PRs.

**Decision:**
1. **Determining the change** uses `/understand-diff`. Against a base branch or a PR number it
   resolves the changed files, maps them to graph nodes, finds the 1-hop *affected* components
   and *touched layers*, scores risk, and writes `.ua/diff-overlay.json` for the dashboard. It
   also checks freshness and *warns* when the graph is stale.
2. **Refreshing the graph** uses `/understand` in *incremental* mode: with the existing
   `knowledge-graph.json` + `fingerprints.json` present, it re-scans/re-extracts only files
   changed since the last recorded commit, then reassembles and validates, rewriting all three
   artifacts. No LLM subagents are required for a normal refresh.
3. **Cadence:** `/understand-diff` when opening or rebasing a stacked PR (to scope the change
   + target review); `/understand` before a PR lands on `master`. The refreshed `.ua/`
   artifacts (`knowledge-graph.json`, `fingerprints.json`, `meta.json`) are committed
   in a commit on the PR-landing branch (before the PR lands on master) so every
   future checkout has a current baseline to diff against.  The author commits
   the refreshed artifacts, not the merge step — a merge commit can't carry
   what the author already wrote on the branch.
4. **Staleness signal:** the graph reflects `meta.json.gitCommitHash` /
   `project.gitCommitHash`; it is stale when `git diff --name-only <graphCommit>..HEAD -- . ':!.ua'`
   (project-scoped, **ignoring `.ua/`** so the artifacts the refresh itself commits
  don't re-trigger staleness forever) is non-empty. `/understand-diff` prints this warning
   automatically — a warning is a "refresh the graph" signal, not a reason to skip the diff.
   Because a squash-merge deletes the feature-branch ref, `gitCommitHash` may no longer exist in
  a fresh clone of `master`, and a raw `git diff <graphCommit>..HEAD` then dies with
   `fatal: bad revision`. `scripts/ua-staleness` (`make check-ua-staleness`) detects that up front via
   `git cat-file -e <hash>^{commit}` and reports it as `unreachable` (treat as stale, run a full
   `/understand`) instead of erroring (CIC-86).
5. **Versioning:** the three artifacts plus `.ua/config.json` and `.ua/.understandignore` are
   checked in; the scratch (`/understand`-generated) material (`.ua/.trash-*/`,
   `.ua/intermediate/`) and the transient `.ua/diff-overlay.json` are git-ignored.

**Rationale:** A current graph is the cheap path to *both* of the project's differentiators —
agent-native reasoning and a reviewable diff overlay — and keeps the per-PR review honest about
cross-layer blast radius. The two-skill split (`/understand-diff` = read/assess,
`/understand` = refresh) matches how the data is produced: a baseline exists, deltas are cheap,
and the expensive full rebuild only runs when no baseline does.

**Enforcement:** recorded in `CLAUDE.md` ("Knowledge graph currency") and in the per-PR
Verified checklist. Any code PR that lands without the graph refresh is a documentation/process
gap, not a correctness bug — but it is the reason downstream consumers drift.

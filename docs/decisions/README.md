# Decision Log — Per-PR Decision Entry Policy

## Background

Prior to this policy, all architectural and security decision notes lived in a single
`CLAUDE.md` file with a `<!-- Reverse-chron order. New entries go ABOVE the existing ones -->`
marker. Every PR that added a decision entry caused a merge conflict at that exact marker line,
and the conflict was unavoidable because all PRs inserted at the same position.

**Root cause**: A single-file, prepend-at-marker pattern with N parallel branches produces
N-1 merge conflicts per cycle. This is an inherent property of the pattern, not a mistake.

## Policy (effective 2026-09-28)

### 1. No `CLAUDE.md` changes in feature PRs
Feature branches must **not** include a diff to `CLAUDE.md`. The decision log in `CLAUDE.md`
is maintained by a single periodic "decision log sync" commit to `master`, driven by the owner
or a dedicated maintainer PR. This commit collects all new `docs/decisions/*.md` entries since
the last sync and appends them (in reverse-chron order) to the decision table in `CLAUDE.md`.

### 2. Per-PR decision entry file
Each PR that introduces a non-trivial architectural, security, or API contract decision **must**
include a new file at:

```
docs/decisions/CIC-NN-short-name.md
```

where `NN` is the Linear ticket number and `short-name` is a 1–3 word snake_case summary.
The file contains the decision entry in the same format that `CLAUDE.md` uses:

```markdown
## [CIC-NN] Short name (date)

[Decision text — the same content that would have gone in CLAUDE.md]
```

The `docs/decisions/` folder is an append-only, per-PR, one-file-per-decision record. It replaces
the "insert at the marker" pattern while remaining a complete, searchable decision log.

### 3. Sync commit
When a batch of PRs merges, the owner (or maintainer) opens a small "decision log sync" PR that:
- Appends all new entries from `docs/decisions/*.md` to `CLAUDE.md`'s decision table (reverse-chron order)
- Is marked `[chore] sync decision log` in the commit message
- This PR **does** modify `CLAUDE.md` — the policy exception is "sync commitments only"

### 4. Enforcement
- The `docs/decisions/README.md` file (this file) is the authoritative policy — it is referenced
  from `CLAUDE.md` itself. Any PR that adds a decision **without** a corresponding
  `docs/decisions/CIC-NN-…md` file is blocked in review.

## Existing entries (pre-policy, for reference)

All pre-`docs/decisions/` entries already live in `CLAUDE.md`. After the first sync commit,
new entries in `docs/decisions/` are the canonical source and `CLAUDE.md` is a generated mirror.

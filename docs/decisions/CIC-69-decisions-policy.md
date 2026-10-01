## CIC-69 — Per-PR decision log policy

**Date:** 2026-09-28

**Problem:** All decision entries previously lived in `CLAUDE.md` with a single
`<!-- Reverse-chron order. New entries go ABOVE the existing ones -->` marker.
Every feature PR that added a decision entry conflicted at the same marker line.
With N parallel branches this produces N-1 unavoidable conflicts per cycle.

**Decision:**
1. New decision entries are created as `docs/decisions/CIC-NN-short-name.md` files,
   one per ticket, in the PR that introduces the decision.
2. `CLAUDE.md` entries below the new policy notice are the **historical record only**.
   No new entries are inserted at the marker.
3. A periodic "decision log sync" PR (owner-only, marked `[chore]`) folds the new
   `docs/decisions/*.md` entries into `CLAUDE.md`'s table in reverse-chron order.
4. Any PR that adds a decision without a `docs/decisions/CIC-NN-*.md` file is blocked
   in review.

**Rationale:** The `append-at-a-single-marker` pattern is inherently serial; even
trivially non-conflicting semantic changes (just adding a new bullet) create a
3-way merge conflict at the marker line whenever two branches touch the same file.
Per-file decisions eliminate the conflict entirely — each PR owns its own file.

**Enforcement:** `docs/decisions/README.md` is the authoritative policy.
The CLAUDE.md notice points to it. The `docs/decisions/` directory is append-only.

# ADR-NNN: <short decision title, stated as the decision>

Status: Proposed | Accepted | Superseded by ADR-NNN · YYYY-MM-DD

## Context

What forces are at play? What constraint, problem, or pressure made this a decision rather than an obvious default? Include the things that will be non-obvious to someone reading this in two years — prior art considered, failure modes of the alternatives, relevant scale or operational assumptions.

## Decision

The decision, in the active voice, as a commitment: "We will …". One decision per ADR. If you find yourself writing "and", consider whether it is two ADRs.

## Rationale

Why this option over the alternatives that were genuinely in contention. Name the alternatives and say what they cost. An ADR that only argues for the winner is not much use later.

## Consequences

What this makes easy, what it makes hard, and what it forecloses. Include the ones you dislike — the honest cost is the reason this file is worth writing. Note any follow-up work or open questions the decision creates.

---

## ADR process

- One decision per file; file names are `ADR-NNN-kebab-title.md` with `NNN` allocated sequentially.
- ADRs are **immutable once accepted**. To change a decision, write a new ADR that supersedes it, and edit the old one only to add the `Superseded by ADR-NNN` marker and a pointer. Never delete an ADR: the record of what was decided and why is the point.
- Small clarifications that do not change the decision may be appended as a dated `## Amendment` section (see ADR-004 for an example).
- Add the new ADR to the index table in `project-docs/README.md` in the same PR.
- A code change that contradicts an accepted ADR needs the superseding ADR in the same PR, or it is not ready.

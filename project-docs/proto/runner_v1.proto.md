# Runner protocol proto — moved

The annotated design draft that used to live here is now the **canonical, frozen**
protocol definition, at:

    /proto/cicado/runner/v1/runner.proto

It was moved rather than copied deliberately: a frozen contract with two copies is
a frozen contract that drifts. The annotations that made the draft worth reading
(the invariants block at the top, the per-field rationale comments) moved with it.

Design rationale and the evolution policy live in
[`../04-runner-protocol.md`](../04-runner-protocol.md) and
[`../adr/ADR-004-runner-protocol.md`](../adr/ADR-004-runner-protocol.md).

Changes are **additive only**: new fields with new tags, `reserved` on removals,
behavior changes via `Capabilities.features` flags. `make proto` runs
`buf breaking` against `main` and CI fails the PR if that rule is violated.

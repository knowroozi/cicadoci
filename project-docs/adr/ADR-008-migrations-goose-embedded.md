# ADR-008: Schema migrations with goose, embedded in the server binary

Status: Accepted · 2026-09-16

## Context

Schema v1 (CIC-7) needs migration tooling, and the choice is hard to reverse: it determines what an operator has to install, what a release artifact consists of, and what happens during a rolling deploy when two instances start at once.

The constraint that matters is ADR-001's one-binary promise. "Operationally simpler than Jenkins" is the project's main claim, and it is made concrete by the install being *one file plus a Postgres URL*. Any tooling that adds a second artifact an operator must obtain, version-match, and run in the right order erodes exactly the thing being sold.

Candidates considered: goose, Atlas, tern, golang-migrate.

## Decision

We will use **goose as a Go library**, with migrations embedded in the server binary via `embed.FS`, applied through `cicado-server migrate up` (and automatically at server start in the default configuration).

Migrations are plain `.sql` files under `server/store/migrations/`, numbered and forward-only in production.

## Rationale

- **Library mode keeps the one-binary promise.** The migrations ship *inside* the binary. There is no migration CLI to install, no directory of `.sql` files to copy to the right place on the host, and no way for the migrations an operator runs to be a different version from the server that runs against them. Upgrading is: replace one file, start it.
- **goose takes a Postgres advisory lock** for the duration of a migration run, so a rolling deploy that starts three instances simultaneously does not race three copies of the same `CREATE TABLE`. This is the property that made the difference; a tool without it would need us to build the locking ourselves.
- **Plain SQL, not a DSL.** The queue schema depends on Postgres specifics — partial indexes matching the claim query's `ORDER BY`, `FOR UPDATE SKIP LOCKED`, a `pg_notify` trigger, expression-based unique indexes over nullable scope columns. These are the design, not incidental detail, and they should be readable as SQL in review.
- **Small dependency footprint**: goose plus pgx adds nine indirect modules. For a project with a dependency audit as an MVP exit gate (M9), that is worth checking before adopting anything.

Alternatives and what they cost:

- **Atlas** — genuinely better at schema *diffing* and drift detection, and its declarative HCL model is attractive. Rejected because the useful parts live in an external CLI, which reintroduces the second artifact. Its inferred migrations are also the wrong shape for hand-tuned partial indexes, which we would end up overriding anyway.
- **tern** — pgx-native and pleasantly small, so it fits the dependency budget. Rejected because it is primarily a CLI; embedding it is not the supported path, and it has no advisory-lock story for concurrent starts.
- **golang-migrate** — the most widely used, and embeddable. Rejected on its driver/dependency sprawl and a history of source-driver interface churn; goose does the same job with less surface.

## Consequences

- Migrations are **forward-only in production**. `Down` exists for local development and is not wired to any deployment path. Rolling a schema back on a live install means writing a new forward migration.
- The binary grows by the size of the migration files — kilobytes, and it buys the version-skew guarantee.
- Anything that inspects the schema without running the server (a support script, a data fix) needs either the binary or a copy of the `.sql` files from the repo. Acceptable: the files are plain SQL in a known directory.
- Adding a migration means adding a file *and* rebuilding, since it is embedded. That is intended — it makes "the binary and its schema travel together" structural rather than a convention people remember.
- If Atlas's drift detection later proves necessary in CI, it can be adopted *as a checker* against the goose-applied schema without changing this decision.

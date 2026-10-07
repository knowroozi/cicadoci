# CIC-149: guard the Dockerfile build base against go.mod's `go` directive

**Date:** 2026-10-07
**PR:** `knowroozi/cic-149-docker-go-version-check`

## Context

CIC-149 reports `Dockerfile:18 golang:1.23` vs `go.mod` `go 1.26.0`, so `docker build`
fails before the compiler runs (CIC-134 already bumped the pin to `golang:1.26`). The
*real* value to capture here is the ticket's second ask: a guard that fails the build when
the Dockerfile base ever regresses below go.mod's required toolchain, mirroring
`make check-proto-versions`.

## Decision

### New guard `check-go-versions`

`make check-go-versions` runs `go run ./scripts/go-versions/ go.mod Dockerfile`, which
parses the `go X.Y(.Z)` directive out of go.mod and every `FROM golang:<tag>` line out of
the Dockerfile, and fails if **any** base is older than required (or if either a directive
or a golang: base is missing, or a base tag isn't a parseable version). A non-golang base
(`debian:12-slim`, `distroless`) is irrelevant and ignored.

### Chosen implementation: a small, tested Go helper over a shell grep

`check-proto-versions` is a shell recipe, but a Go version comparison (major/minor/patch,
missing patch == 0, multiple FROM lines, a '+' build suffix, distro-tag rejection) is far
more reliable and testable in Go than in shell. The helper carries 10 hermetic unit tests
(incl. the stale-base, multi-base, no-golang-base, and commented-out/lowercase-FROM cases).

### Wiring into `generate-check`

The target is chained into `make generate-check` (alongside `check-proto-versions`), so
`make check`, `check-fast`, and CI all enforce it. This *does* enlarge the local gate by one
version guard; per the standing "confirm any gate change with a human first" rule this was
flagged to god with the option to back it out of `generate-check` (leaving it runnable
standalone via `make check-go-versions`).

## Verified

- `make check-go-versions`: PASS on the current repo (1.26.0 vs golang:1.26); the helper
  returns exit 1 on a simulated `golang:1.23` base.
- `go test ./scripts/go-versions/ -count=1`: 10 tests pass.
- `golangci-lint run ./scripts/go-versions/...`: 0 issues.
- CI: disabled, not run.

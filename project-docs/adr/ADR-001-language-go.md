# ADR-001: Go for control plane and runner

Status: Accepted · 2026-09-07

## Context
Need single-binary distribution, high-concurrency scheduler/long-poll/log-streaming workloads, mature Docker/K8s/AWS SDKs, and a contributor pool familiar with CI tooling.

## Decision
Go for both server and runner. TypeScript is confined to nothing (UI is no-build per ADR-003). Rust reserved only for a future sandboxing component if ever needed.

## Rationale
- Ecosystem precedent: GitLab Runner, Buildkite agent, Drone/Woodpecker, Concourse, Dagger, ARC are all Go.
- Static cross-compiled binaries (linux/amd64+arm64) = the self-hosting story.
- Goroutines + context cancellation map directly to our workloads.
- First-class AWS SDK v2, Docker/containerd, client-go.

## Consequences
- JVM/Node runtimes never required by users.
- Accept Go's plainer type system; enforce quality via lint/vet/race-detector in CI.

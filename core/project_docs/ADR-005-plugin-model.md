# ADR-005: Container plugins with capability manifests and signing

Status: Accepted (execution model in MVP; marketplace post-MVP) · 2026-09-07

## Context
Jenkins' in-process plugins are the central anti-pattern (crash/compromise the controller). Drone/Woodpecker proved plugins-as-containers (settings via env vars) — simple, language-agnostic, isolated by construction.

## Decision
A plugin is an OCI image + manifest declaring inputs and required capabilities (network, secrets, mounts — deny by default). Referenced by digest; cosign signature verification required for the (post-MVP) verified tier. Never any in-process third-party code in the control plane. Post-MVP: Wasm (Extism) tier for lightweight in-process-style hooks with capability-scoped host functions; gRPC (hashicorp/go-plugin style) reserved for trusted first-party control-plane extensions.

## Rationale
- Inherits container isolation for free; aligns with the docker executor.
- Digest pinning + signing directly answers the tag-repointing attack class (tj-actions CVE-2025-30066).
- Drone-compatible env-var convention can inherit an existing plugin ecosystem.

## Consequences
- Requires a container runtime on runners (already true).
- Capability enforcement lands in the executor; manifest schema versioned.

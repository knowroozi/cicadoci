# Security Policy

## Reporting a vulnerability

**Do not open a public GitHub issue for a security vulnerability.**

Report privately via GitHub's [private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability) on this repository, or email **security@cicadoci.com**.

Please include: what you found, how to reproduce it, the impact you believe it has, and whether it is already public. We aim to acknowledge within 72 hours and to ship a fix or a documented mitigation before any public disclosure.

Pre-1.0, CICaDo has no formal support window: fixes land on `main` and in the next tagged release.

## Scope

CICaDo is a CI/CD system: it runs user-supplied code by design. That makes the interesting boundary not "can a job run code" — it can, that is the product — but whether a job can cross out of its own blast radius.

In scope, and taken seriously:

- **Secret disclosure** — any path by which a secret value reaches a log, an artifact, an API response, a journal, a job assignment, or a job that should not have it. Redaction bypasses (novel encodings, chunk-boundary splits, timing) are in scope.
- **Cross-tenant access** — one org, repo, or run reading or affecting another's data, logs, artifacts, or runners.
- **Privilege escalation** — a member acting as an admin; a job token acting outside its run/repo/branch claims; a runner credential claiming jobs it was not assigned.
- **Authorization bypass on any surface** — web, Slack, MCP, or API. Slack interactive payloads authorizing the *message* rather than the *acting user* is a specific class we care about.
- **Fork-PR isolation escapes** — an untrusted fork PR obtaining secrets, a deploy identity, or a privileged runner tag.
- **OIDC issuance flaws** — identity tokens with claims that do not match the run, or that outlive it.
- **Control-plane compromise from a job** — anything letting job-controlled input execute in the server process.

Out of scope:

- A job running arbitrary code on a runner it was legitimately assigned to. That is the intended behavior. The `shell` executor in particular shares host state between jobs by design and is documented as trusted-jobs-only.
- Findings that require an already-compromised control plane, database, or operator credential.
- Missing hardening headers, rate limits, or best-practice deviations with no demonstrated impact.
- Denial of service through resource exhaustion on a self-hosted install the reporter controls.
- Vulnerabilities in a dependency with no reachable path in CICaDo.

## Security invariants

These are load-bearing. A bug that breaks one is a vulnerability even without a full exploit chain:

- Secret values are delivered only through `GetJobSecrets`, authorized against the live lease — never in assignments, journals, or anything redeliverable.
- Redaction runs on log ingest, before persistence and fan-out.
- No long-lived cloud keys: jobs federate to AWS via OIDC `AssumeRoleWithWebIdentity`.
- Fork PRs receive no secrets, no deploy identities, and restricted runner tags.
- Every state-changing action from any surface writes an audit event.
- No third-party code executes in the control-plane process ([ADR-005](project-docs/adr/ADR-005-plugin-model.md)).

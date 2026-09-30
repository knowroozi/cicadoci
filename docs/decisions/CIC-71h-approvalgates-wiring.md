# CIC-71h — Approval gates + chat integration in controller

**Ticket:** CIC-71 (sub-blocker 8, final)
**PR:** `knowroozi/cic-71h-chat-wiring`
**Date:** 2026-09-28
**Branch base:** `knowroozi/cic-71g-backpressure-wiring` (stacked)

---

## Decision

Add `*approvalgates.Manager` to `controller.Config` and call
`maybeOpenGate` at the end of `HandlePullRequest` for every PR run.
The gate is opened as a placeholder — a full policy layer that
decides which runs require approval is a future ticket.

## `nil`-safe wiring

`maybeOpenGate` short-circuits when `c.cfg.ApprovalGates` is nil.
This means that:
- Dev/test servers that don't configure a Slack (or other CI chat)
  provider continue to work without any change.
- A future `DefaultConfig()` can inject a real `approvalgates.Manager`
  without breaking any existing caller.

## What `maybeOpenGate` does (MVP placeholder)

For every `pull_request` webhook, `maybeOpenGate` creates a
`gate` with:
- `RunID`: the run's UUID
- `Channel`: PR number as string (used as a Slack channel name)
- `Reason`: human-readable "PR #N on org/repo"
- `Allow`/`Deny`: "approve"/"deny" action IDs

The `approvalgates.Manager.Open` call posts the gate to the chat
provider.  The `onResolved` callback (wired by the server, not this
file) advances the run from pending to running when a user clicks
"approve", or cancels it on "deny".

### Scope

MVP has one gate per PR with no policy: every PR run gets a gate.
A future ticket should gate on a policy layer — e.g. only PRs that
touch deploy files or target the `main` branch get a gate.

## `Config.ApprovalGates` field

```go
// ApprovalGates, when non-nil, opens a deployment-approval
// gate for runs that opt in via a policy.  The onResolved callback
// (wired by the server) advances the run.  nil is a no-op.
ApprovalGates *approvalgates.Manager
```

`DefaultConfig()` does NOT initialise `ApprovalGates` — the server
startup (`main.go`) will construct an `approvalgates.Manager` with
a `chat.ChatProvider` and pass it to `controller.Config`.

## Scope

### Done
- `Config.ApprovalGates *approvalgates.Manager` field
- `maybeOpenGate` helper
- Called at end of `HandlePullRequest`
- `nil`-safe
- Test: `TestMaybeOpenGate_NilManager`
- `golangci-lint` clean; all tests pass

### Deferred
- `DefaultConfig()` wiring of `approvalgates.Manager` + `chat.ChatProvider`
    (needs Slack app credentials; done in server startup, not here)
- Policy layer: which runs need a gate
- `Main()` wiring of `approvalgates.Manager` into `controller.Config`

### Not part of this ticket
- `chat.SlackProvider` implementation (CIC-43)
- Slack OAuth + signing (CIC-43)
- Chat message rendering for step progress / status (CIC-44)

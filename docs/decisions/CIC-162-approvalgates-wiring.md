# CIC-162 (part 1) — Build the approval-gates Manager in cicado-server

**Branch:** `knowroozi/cic-162-approvalgates-wiring` (based on `origin/master`, after CIC-160 / #129)
**PR:** #135 — `Refs CIC-162 (part 1)`

## Scope

`server/approvalgates` was on `master` but inert: `controller.DefaultConfig()`
leaves `ApprovalGates` nil, so `maybeOpenGate` (`controller.go`)
short-circuits and no approval gate ever opens in the shipped binary.

This PR is **part 1** of CIC-162 (rescoped by god after review): it only
constructs the `Manager` in `main.go` and audits gate transitions. The rest
of the ticket's acceptance is a follow-up ticket:

- gating on a workflow-declared approval (`maybeOpenGate` still opens a gate
  for every PR run);
- holding dependent jobs `pending` until approval, failing them on deny/timeout;
- a caller of `Manager.Resolve` (Slack interaction handler, CIC-138, and an
  audited API endpoint);
- auditing rejected unauthorized clicks;
- the e2e test.

## What changes

1. `chat.NoOpProvider`: a `ChatProvider` that contacts no external service.
2. `--approvalgates-enabled` (`CICADO_APPROVALGATES_ENABLED`): builds a
   NoOp-backed `approvalgates.Manager` and wires it into
   `ctrlOpts.ApprovalGates`.
3. `--insecure-approvalgates-noop` (`CICADO_INSECURE_APPROVALGATES_NOOP`):
   required alongside `--approvalgates-enabled`; without it startup is refused.
4. `--approvalgates-allowed` (`CICADO_APPROVALGATES_ALLOWED`): comma-separated
   user IDs → `ctrlOpts.AllowedApprovers` (empty = fail closed, warned).
5. Channel and timeout reuse CIC-160's `--approval-channel` /
   `--approval-timeout` (`Config.ApprovalChannel` / `ApprovalTimeout`).
   There is no separate approvalgates channel flag.
6. `onResolved` writes one audit entry per terminal transition, via
   `gateAuditEntry`.
7. `audit.ActionDeny` and `audit.ActionGateTimeout` added;
   `Manager.ActorID(runID)` added; `GateState.String()` added.

## Decisions

- **No real Slack provider yet → the NoOp is a development-only path.**
  With the NoOp, nothing is posted and nothing can call `Resolve`, so every
  gate just times out. Running that silently in production would only produce
  inert gates, so `--approvalgates-enabled` fails closed unless the operator
  also passes `--insecure-approvalgates-noop` (same pattern as
  `--insecure-api` / `--insecure-webhook`), and startup logs a Warn that the
  gates are inert.
- **Audit action reflects the outcome.** `GateApproved` → `approve`,
  `GateDenied` → `deny`, `GateTimedOut` → `gate_timeout`. A consumer filtering
  on `action = 'approve'` sees approvals only. The actor is the user who
  resolved the gate (`GateRecord.ActorID`, read through `Manager.ActorID` —
  `onResolved` runs outside the manager's lock); a timeout has no acting user
  and is recorded as `system`.
- **Unauthorized clicks are NOT audited here.** `Manager.Resolve` rejects an
  actor outside `Gate.Allowed` and returns an error; it does not import
  `audit`, and its doc says the *caller* should audit the rejection. There is
  no `Resolve` caller yet, so that audit lands with the caller (follow-up).
- **Fail closed on empty `--approvalgates-allowed`.** A gate opens but no one
  can resolve it; a Warn is logged. No channel fallback (CIC-160).
- **`GateState.String()`** returns `GateState(N)` for unknown values so a bad
  state is not disguised as `pending`.

## Tests

- `TestApprovalgatesFlagsAreWired` — fail-closed defaults; no
  `--approvalgates-channel` flag.
- `TestSplitAllowList` — comma split, whitespace trim, empty entries dropped.
- `TestBuildApprovalGates` — disabled → nil; enabled without the NoOp ack →
  startup error; enabled → Manager + parsed allow-list; a deny writes an audit
  row with `action=deny` and the acting user.
- `TestGateAuditEntry` — state → action/actor mapping.

## Security / invariants

- A gate resolution is authorized in `Manager.Resolve` → `authorized()`
  against `Gate.Allowed`, independent of the provider.
- Every terminal gate transition writes an audit event with the right action.
- No secret values logged.

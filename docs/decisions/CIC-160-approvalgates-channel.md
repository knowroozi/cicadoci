# CIC-160 — approvalgates Channel source

## Background

The `approvalgates.Gate.Channel` field was set to
`fmt.Sprintf("%d", payload.PullRequest.Number)` in
`controller.maybeOpenGate` — i.e., a string representation of the PR
number (e.g., `"42"`).  This value is used as the Slack channel
identifier when `manager.Open` calls
`provider.RequestApproval(ctx, g.Channel, ...)` to post the approval
message.  PR numbers are not valid Slack channel identifiers, so no
approval message was ever posted to a real channel.

The fix is to use a configured `ApprovalChannel` field on the controller
`Config`, and fail-closed (B2) when empty: no gate is opened.

## Change

### `server/controller/controller.go`

1. `Config.ApprovalChannel string` — Slack channel ID/name for gate messages.
2. `Config.ApprovalTimeout time.Duration` (B1) — how long a gate stays open;
0 = no auto-timeout.  Set from `--approval-timeout` in main.go.
3. `maybeOpenGate` fail-closed (B2): if `ApprovalChannel` is empty, the gate is
NOT opened and a warning is logged instead of falling back to `"#general"`.
4. `validateApprovalChannel(channel string) error` — sentinel
   `errNoApprovalChannel` when channel is empty.  No `#general` fallback.
5. `approvalgates.Gate.Channel` wired from `Config.ApprovalChannel` (B1);
   `Gate.Timeout` wired from `Config.ApprovalTimeout` (B1).
6. `errNoApprovalChannel = fmt.Errorf("approvalgates: no approval channel
   configured")` — sentinel used by `validateApprovalChannel`.

### `server/controller/controller_test.go`

- `TestValidateApprovalChannel_EmptyFails` — B2: empty channel is an error.
- `TestValidateApprovalChannel_NonEmptyOK` — non-empty channels all pass.
- `TestMaybeOpenGate_SkipsWhenNoChannel` — empty channel: gate NOT opened.

### `server/cmd/cicado-server/main.go`

- `--approval-channel` flag (env `CICADO_APPROVAL_CHANNEL`),
   default empty (no gate opened when unset — fail-closed).
- `--approval-timeout` flag, default `0s` (no auto-timeout).
- Wired to `ctrlOpts.ApprovalChannel` / `ctrlOpts.ApprovalTimeout`.

## B2 decision (god conv-13-04Z)

No `#general` fallback.  If no channel is configured, do NOT open the
gate.  Log a warning.  Never post approval requests to a public or
default channel.  This is safer than a `#general` fallback which could
leak sensitive approval information.

## Security / behaviour

- `Config.ApprovalChannel` is operator-supplied via `--approval-channel`
   or `CICADO_APPROVAL_CHANNEL`.  When empty, the gate is NOT opened
   (fail-closed).
- `Config.AllowedApprovers` comment in `controller.go` is unchanged:
   it was NOT part of this fix despite a previous commit message claim.
- `ApprovalGates` is still `nil` in `DefaultConfig`; no gate opens in
   the shipped binary until CIC-162 wires the tracker construction,
   the `Resolve` caller, and dispatch blocking.
- `validateApprovalChannel` is a pure function with no side effects,
   making mutation-proof testing trivial.

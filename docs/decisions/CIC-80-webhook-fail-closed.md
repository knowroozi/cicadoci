# CIC-80 — Webhook HMAC verification fails closed on an empty secret

**Ticket:** CIC-80 (High, security)
**PR:** `knowroozi/cic-80-webhook-fail-closed`
**Date:** 2026-09-30
**Branch base:** `master`

---

## Decision

When `CICADO_WEBHOOK_SECRET` is unset, the webhook receiver must **fail
closed** — every delivery is rejected with 401 — unless the operator
explicitly passes `--insecure-webhook` (env `CICADO_INSECURE_WEBHOOK`,
default `false`). The previous behaviour of silently skipping HMAC
verification when the secret was empty is unsafe for a deployed server
because a single configuration misslet (unset secret, forgotten flag,
empty env var in a CI image) turned a security control into a no-op
with no signal.

## Two layers of guarding

The fix has two complementary layers:

1. **Receiver fails closed per-request.** A `Receiver` constructed
   with `Secret = ""` and `AllowUnauthenticated = false` rejects
   every incoming delivery with 401, regardless of the signature
   header. This is enforced inside `ServeHTTP` so it cannot be
   bypassed by a future call site that forgets to check in
   `main.go`.

2. **`main.go` refuses to start.** `cicado-server` returns a clear
   error and exits when the webhook secret is empty and `--insecure-
   webhook` was not passed: `"CICADO_WEBHOOK_SECRET is not set and
   --insecure-webhook was not passed; set CICADO_WEBHOOK_SECRET for
   signature verification, or pass --insecure-webhook (fail-open;
   for local development only)"`. This mirrors the existing
   `--allow-ephemeral-key` refusal pattern and matches the
   owner's "fail closed behind an explicit dev flag" instruction.

## `ReceiverOptions.AllowUnauthenticated`

New field on `ReceiverOptions`, defaulting to `false`. When `true`
and `Secret` is empty, the receiver logs a `Warn`-level message
("webhook receiver running WITHOUT signature verification (safe for
local development only)") and accepts unauthenticated deliveries.
This is a development convenience; it is **not** safe for a server
with external network exposure.

## Test updates

- `TestReceiver_NoSignatureCheck_WhenSecretEmpty` was renamed to
  `TestReceiver_NoSignatureCheck_WhenUnauthenticated` and updated to
  set `AllowUnauthenticated: true`.
- `TestReceiver_FailsClosed_WhenSecretEmpty` asserts that an empty-
  secret, non-opted-in receiver returns 401 for a push delivery and
  does not call `HandlePush`.
- `TestReceiver_WarnsOnUnauthenticatedConstruction` captures the
  logger output and asserts that the "WITHOUT signature verification"
  message is emitted.

## Files changed

- `server/webhook/receiver.go` — `AllowUnauthenticated` option,
  per-request fail-closed check, loud startup warning
- `server/cmd/cicado-server/main.go` — `-insecure-webhook` flag
  (env `CICADO_INSECURE_WEBHOOK`), startup refusal, `AllowUnauthenticated` passed to `NewReceiver`
- `server/webhook/receiver_test.go` — three new/updated test cases

## Non-obvious notes

- The `else if !r.allowUnauthenticated` branch in `ServeHTTP` is
  the "fail closed at the receiver" layer; it rejects unauthenticated
  deliveries even when `--insecure-webhook` is **not** passed.
- The `main.go` refusal is the "fail at startup" layer; it prevents
  the server from even reaching its listen loops if the secret is
  absent.
- Both layers are needed: the per-request guard protects any future
  `NewReceiver` call site; the main.go refusal is the operator-facing
  guard that prevents silent acceptance of unauthenticated deliveries
  in a misconfigured or accidentally-empty-secret deployment.

## [CIC-82] Wire live log streaming: SSE route + LogPublish (2026-10-02)

The QC audit found built subsystems the server never wired: the per-job SSE
log handler (`server/logs`) had no route, and `runnersvc.Config.LogPublish`
was never set, so no live log line reached anyone.

### Decision

- **Route.** `api.WithSSEHandler` serves `GET /api/v1/logs?job=<id>`. Without
  the option the route does not exist.
- **Its own chain, still authenticated.** The stream runs through
  `buildSSEChain`: auth → logging → recover. It drops `withRequestTimeout`
  (which would cut every stream at 30 s) and `withJSONErrors` (which buffers the
  whole body, so nothing would stream). Every other request, and every other
  method on `/api/v1/logs`, still takes the full `buildChain`.

> **Known issue: CIC-150.** "still authenticated" describes the chain *shape* (`buildSSEChain` keeps the `buildChain` auth slot), but at runtime **no real authenticator is wired**: `main.go`'s `api.New` (main.go:270) supplies no authenticator (`AllowUnauthenticated`, main.go:266, is webhook-only), so the SSE stream — like the rest of the API — is currently readable by anyone. The slot exists; a real authenticator is not yet supplied (CIC-150).
  `statusRecorder` forwards `Flush` so events pass through `withLogging`.
- **Masked fan-out.** `LogPublish` gains a `data []byte` argument carrying
  exactly the bytes persisted to `log_entries`, which have already been through
  the masker. Live viewers see the same redacted text as the stored log.
- **main.go** builds a `logs.Hub` and an SSE handler whose history replay reads
  through `archive.Compactor`, backed by an in-memory blob store until an S3
  store is configured.

### Backpressure moved out of this ticket

CIC-82 originally also turned on the dispatcher's backpressure tracker. That
tracker `Acquire`s per job and never releases on completion, so job 5+ of every
run stalled. Run-level backpressure is now CIC-106 (#102): the controller holds
one slot per run and releases it from the single run-terminal hook, and the
dispatcher deliberately gets no tracker. This PR does not touch backpressure.

### Known limits

- The SSE handler sends its headers with the first event, so a client of a
  silent job waits for headers. `EventSource` handles that; a plain HTTP client
  with a short header timeout may not.
- `logs.writeEvent` writes chunk bytes as one `data:` field. In a chunk with
  several lines, everything after the first newline lacks the `data:` prefix
  and an `EventSource` client drops it. The fix (one `data:` line per line)
  belongs to the logs package and is a follow-up, not part of this wiring.

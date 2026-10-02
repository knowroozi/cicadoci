# CIC-83 Disposition: Stale / Misattributed TODOs + Lambda Stub

**Status:** In Review — PR #93 (CIC-83)
**Opened:** 2026-10-01
**Parent:** QC audit of master @ c8d89fc (Dwight 8751dc)

## The question

A QC audit on master @ c8d89fc flagged stale or misattributed todos and an
abandoned lambda stub. No functional gap — no test break, no CI fail — but
the comments point at the wrong tickets or describe work that is done or
in-flight on open PRs.

## Disposition of each flagged item

### `server/controller/controller.go:267,279` — `TODO(CIC-71d): wire re-check`

**Status:** superseded by CIC-81 (PR #92, `knowroozi/cic-81-forge-wiring`).

Re-run handling belongs to CIC-81 (PR #92, still open). On this branch the
`TODO(CIC-71d)` comments are replaced by accurate no-op comments: the
handlers log and return; no run is created.

### `server/controller/jobs.go:220` — `TODO(CIC-38/39/40): populate when per-job secret references land`

**Status:** resolved by CIC-79 (PR #91, merged).

`secretRefsFor` now extracts the distinct, sorted list of secret names from
job-level and step-level env. The `SecretRefs` field on the `JobSpec` is
populated when the job is created. No action needed on master.

### `server/runnersvc/logstream.go:317` — `TODO(CIC-37): masker scrubs against an empty registered-value set`

**Status:** resolved by CIC-79 (PR #91).

The masker is now populated when `secret_refs` are set on a job: the runner's
`secrets.go` registers each fetched secret value with the masker's value set
before log streaming begins. The `logstream.go` comment is updated to
describe the new behaviour. No additional ticket needed.

### `server/api/handler.go:219,226` — `TODO(M3/M5): latency histogram metric`

**Status:** legitimate future work, keep.

The `withLogging` helper currently emits a per-request access log line. The
real latency histogram is tracked as CIC-136, so the TODOs now read
`TODO(CIC-136)`.

### `runner/lambda/lambda.go:104` — `TODO: wire to gRPC queue connect + job fetch + execute`

**Status:** stub, now fails closed; real handler tracked as CIC-137.

The `lambda.Handler.Dispatch` function records the dispatch event in the
journal but never runs the job. It used to return `Success: true`, so any
caller would have marked every job as passed without executing anything.
It now fails closed: it returns `Success: false`, `ExitCode: 1`,
`FailureClass: "infra"` and `Message: NotImplementedMessage` ("lambda
executor is not implemented ..."), and logs a warning. `JobResponse` gains
the additive `Message` field to carry that reason. Nothing outside the
package calls `Dispatch`; only its own test depended on the old result, and
`TestDispatch` now asserts the failure.

The real path — connecting to the gRPC queue, fetching the job, executing
it — requires a public RPC surface on the queue (a `DispatchJob` or
`SubmitJob` RPC) that does not exist in MVP. The comment now reads
`TODO(CIC-137)` and explains why the stub fails closed.

### Misattributed `CIC-38/39/40` references

CIC-38/39/40 are the secrets RPC, the OIDC issuer and per-job tokens. The
comments that cited them for GitHub work now point at the right tickets:

- `controller.go` package doc and `resolveRepo`: installation-ID wiring and the
  GitHub forge are CIC-81; per-job installation tokens are CIC-26.
- `jobs.go` `WorkflowLoader` doc: the production workflow producer is CIC-84.
- `noop.go`: the real `githubForge` is CIC-81.

## What is NOT in scope

- The `logstream.go` masker TODO: resolved by PR #91 (CIC-79, merged).
- Implementing the latency metric (CIC-136) or the real Lambda handler
  (CIC-137).
- The `Handler.Execute` method or any other lambda adapter logic.

## Verification

- `go build ./...` clean
- `go vet ./...` clean
- `golangci-lint run ./...` — 0 issues
- `go test -race -short ./runner/lambda/` passes

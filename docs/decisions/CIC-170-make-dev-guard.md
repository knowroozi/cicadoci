# CIC-170: make dev broken after CIC-150

_Creed's QC pass 7 flag. High priority._

## Decision

After CIC-150 (PR #116), `server/cmd/cicado-server/main.go` calls
`apiStartupGuard` at startup, which returns an error when neither
`CICADO_API_TOKEN` nor `--insecure-api` / `CICADO_INSECURE_API` is
set.

`docker-compose.yml` set `CICADO_INSECURE_WEBHOOK: "true"` but did
**not** set `CICADO_INSECURE_API: "true"`.  As a result, `make dev`
started the server process, the gRPC listener came up, but the
HTTP listener's `apiStartupGuard` refused to start when
`CICADO_API_TOKEN` was unset.

## Fix

1. **docker-compose.yml**: add `CICADO_INSECURE_API: "true"` to the
   `server` service environment, after `CICADO_INSECURE_WEBHOOK`.
   Same rationale: the dev stack has no real secrets.

2. **main.go:180-189**: remove the duplicated "Construct the secrets
   store" comment block.  It was accidentally doubled during the
   CIC-159 rebasing.

3. **compose_guard_test.go**: new unit test that asserts
   `docker-compose.yml` contains `CICADO_INSECURE_API: "true"`.
   Prevents future drift.

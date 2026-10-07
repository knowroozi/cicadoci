# CIC-212: docker multi-line env denylist + runner-env override guard

**Date:** 2026-10-07
**PR:** #156 `knowroozi/cic-212-docker-env-denylist`

## Context

`RunStep` (runner/executor/docker.go) appends multi-line `K=value` to
`cmd.Env` after `os.Environ()`. That is the environment of the **docker
CLI process**; Go's `exec` keeps the last duplicate, so a step's value
overrides the runner's own value for that key. A denylist was in place
for some Docker/proxy/SSL keys, but glibc loader, macOS linker, and Go
runtime keys were not on it (CIC-212).

## Decision

### B1 — runner-env override guard

In the multi-line branch of `writeEnvFile`, before the denylist check:
`if _, set := os.LookupEnv(k); set { return error }`. This is the
ticket's primary rule — no fixed denylist can list every operator-supplied
secret (KUBECONFIG, SSH_AUTH_SOCK, AWS_*, GOOGLE_APPLICATION_CREDENTIALS,
…). A step can never override a key that the runner already has set.

### B2 — denylist correction

**Added (Go runtime semantic vars, replacing toolchain-only ones):**
- `GODEBUG`, `GOTRACEBACK`, `GOGC`, `GOMEMLIMIT`, `GOMAXPROCS`
  These five are the knobs the Go runtime of a compiled binary reads
  at process start.

**Removed (Go toolchain vars, read only by `go build`/`go mod`):**
- `GOFLAGS`, `GOEXPERIMENT`, `GOMODCACHE`
  The docker CLI is a compiled binary and the Go runtime does not read
  these at process start. They are not a security concern for the CLI
  process. If a runner has one of these set, B1 (os.LookupEnv check)
  blocks the override.

### S1 — glibc UNSECURE_ENVVARS

Added from glibc's own `UNSECURE_ENVVARS` list (glibc ignores these
in secure/setuid mode; the docker CLI binary may be setuid on some
distros):
- `LOCPATH`, `NLSPATH`, `MALLOC_TRACE`, `HOSTALIASES`, `LOCALDOMAIN`,
  `RES_OPTIONS`, `RESOLV_HOST_CONF`, `GETCONF_DIR`, `NIS_PATH`, `TZDIR`

### Comment restructure

- `cliEnvDenylist` comment: broken "Three families" nesting fixed;
  now organized by family with clear bullet structure.
- `writeEnvFile` D1 comment: removed "legitimate operator-configured
   env var" wording (step env comes from the workflow file / PR author,
   not the operator). Now says "step env goes into the --env-file, not
   cmd.Env, so single-line keys never reach the host docker CLI."
- `cliEnvPrefixDenylist` comment: "Two families" → "Three families"
  (DOCKER_, LD_, DYLD_ = three).
- `TestWriteEnvFile_DenylistLoaderKeys` no-op `t.Logf` block: replaced
  with assertion that error text contains the key name.

## Future improvement

The preferred long-term mitigation (see CIC-210) is to mount step
env as a 0600 file with `KEY_FILE` for multi-line keys, never putting
a value in `cmd.Env` at all. That would eliminate the entire surface
without a denylist. This ticket is the pragmatic Low-priority
denylist + B1 guard in the meantime.

## Gate

`golangci-lint run ./...` 0 issues. Full `go test -race -count=1 ./...`
PASS incl. `test/e2e`. Tests: `TestCliEnvDeny`, `TestCliEnvNotDenied`,
`TestWriteEnvFile_DenylistLoaderKeys`, `TestWriteEnvFile_DenylistKeys`,
`TestWriteEnvFile_RunnerEnvGuard` (new B1 test).

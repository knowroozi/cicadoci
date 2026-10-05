# CIC-195, CIC-196: Docker executor container naming + secret isolation

## CIC-196 — Security: secrets in argv

### Problem
`docker run -e KEY=VALUE` places the literal secret value in the `docker`
client's argv, readable via `/proc/<pid>/cmdline` by any local user.

The earlier CIC-196 pass put step env into `cmd.Env` (the docker client's
own process environment) and passed `-e KEY` (bare, no value). This had two
problems:

1. **B1 (SECURITY):** a workflow could set `DOCKER_HOST=tcp://attacker:2375`
   in `step.Env`, and since that key lands in the client's process env,
   every subsequent docker operation (`docker run`, `docker pull`, `docker
   rm -f`, `--env-file` resolution) redirects to an attacker controlled
   daemon. The same applies to `PATH`, `*_PROXY`, `NO_PROXY`, `XDG_*`,
   `SSL_CERT_*`, `HOME`, and `DOCKER_CONFIG`.
2. **B2:** a broken host-env loop (`kv[i-1] == '='`) did not inherit
   `os.Environ()`; it forced `HOME` to `os.TempDir()`, breaking rootless
   docker and registry auth.

### Fix (B1, B2)
- `dockerRunEnv(step, envFile)` — the docker client's env is `os.Environ()`
   only. No step env, no hardcoded overrides. B2 is fixed by using
   `os.Environ()` directly.
- `writeEnvFile(workspace, step.Env)` — writes `step.Env` as
   `KEY=VALUE` lines to a 0600 file in a 0700 temp dir under the
   workspace. Returns the file path and a cleanup function.
- `dockerRunArgs` accepts an `envFilePath` argument. When non-empty,
   `--env-file <path>` is added to the docker run command line.
- `containerName(jobID string)` now takes a job ID to support B4.
- `TestDockerRunArgs_NoSecretsInArgs` verifies that no `-e` flags appear
   in the argv, that secret values do not appear, and that
   `--env-file <path>` is present.

## CIC-195 — Container cleanup on timeout/cancel

### Problem
CIC-191 (#136) killed the `docker` client process group on timeout/cancel
but left a still-running `--rm` container under the daemon.

### Fix
- **B4:** `containerName(jobID)` uses a process-wide atomic counter
   (`containerSeq` via `sync/atomic`) plus 8 random hex bytes plus the
   job ID. Container names are unique across all concurrent jobs, so one
   job's `killContainer` cannot remove another job's container.
- **B3:** `killContainer(ctx, name)` uses `context.Background()` when the
   passed context is already cancelled, so `docker rm -f` runs on cancel/
   timeout. The old code passed the cancelled job context and `docker rm -f`
   never ran.
- `--name <cName>` is in `dockerRunArgs`; `killContainer(ctx, cName)` is
   called after `cmd.Wait()`.
- `TestDockerRunArgsIncludesName` verifies `--name` presence.
- `TestDockerTimeoutKillsStep` confirms a hung step is killed promptly.

### B3 cancel-path test
The existing `TestDockerTimeoutKillsStep` (using the fake docker) confirms
that when the step times out, the container teardown runs. A
cancel-path test would require a real daemon. The fake docker does not
actually create containers, so it cannot be a full B3 proof. This is
documented as a known gap.

## Known limitations (B5)
- No `-tags docker` CI step for real `docker_real_test.go` coverage
   (timeout cleanup, ENTRYPOINT, `--network none`). The `docker_real_test.go`
   file exists but has no `//go:build docker` gate that enables it in CI.
   The acceptance criterion for full CIC-195 close is pending a CI
   pipeline with docker access.

## Out of scope
- Persistent container tracking via the runner service.
- A `CICADO_EXECUTOR=docker` gate in the controller's `NewExecutor` call path.
- Real-daemon CI coverage (see B5 above).

## References
- Linear: CIC-195, CIC-196
- Related: CIC-191 (#136, client group kill), CIC-152 (`--entrypoint ""`)

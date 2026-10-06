# CIC-210: Multi-line env values in the docker executor

## Problem

`writeEnvFile` rejected any `step.Env` value that contained a `\n` or `\r`.
Multi-line values - PEM/SSH-key bundles, X.509 certificates - are common in
CI/CD workflows. The shell executor passed them through `cmd.Env` without
issue; the docker executor rejected them, so any run that needed a
multi-line secret on a `docker` step would fail.

## Decision

Docker's `--env-file` format is one `KEY=value` record per line, delimited by
`\n`. A literal newline in the value truncates the record and corrupts every
subsequent line. Multi-line values therefore cannot be represented in an
`--env-file` and must use a different mechanism.

### B1 fix: keys-only in `--env K`, value in `cmd.Env`

The first implementation (r1 B1) emitted `--env KEY=value` - the full
value in argv, visible via `ps aux`. That reopened CIC-196 B1.

The r3 fix: multi-line values are passed via `--env K` (key only, no
value in argv). The value is appended to `cmd.Env` after `os.Environ()`,
exactly mirroring the shell executor. Docker's CLI reads environment
variables from the process's own environment, so `cmd.Env` is the
right place. No value appears in argv.

### Split path

1. **Single-line values** flow through the existing `--env-file` path
   (CIC-196 B1/C1): a `0600` file in an `OS temp` dir, not visible in
  the process listing. No change.

2. **Multi-line values** are passed via `--env K` (key only) in the docker
   CLI argv. The `K=value` pair is appended to `cmd.Env` after
   `os.Environ()`. No value appears in argv - `ps aux` shows only
   `--env K`, never `--env K=<value>`.

### B2: CLI env denylist

#### Problem

`writeEnvFile` would write ANY `step.Env` key into the `--env-file`, even
keys that carry the host runner's secrets or critical runtime context:
`DOCKER_HOST`, `HOME`, `PATH`, `TMPDIR`, proxy variables, SSL cert
paths. A malicious or misconfigured workflow step could set a multi-line
`DOCKER_HOST` value to route outbound proxy traffic.

Single-line denylisted keys (e.g. `HTTP_PROXY=http://proxy:8080`) are
a problem only if they are multi-line: their values never reach the
host docker CLI's environment because the docker CLI reads from
`cmd.Env` (where they are not present) or `--env-file` (which they are
not routed to for single-line values - they go straight to the file,
not to `cmd.Env`). A multi-line denylisted key, however, would be
passed via `--env K` with its value in `cmd.Env`, exposing it to the
container.

#### Decision

A **CLI env denylist** rejects multi-line denylisted keys with an error:
`writeEnvFile` returns `fmt.Errorf("denylisted multi-line env key %q", k)`.
Single-line denylisted keys are allowed through the env file - they never
reach the host docker CLI's env.

The denylist applies only to multi-line values: `cliEnvDenied(k)` is
checked inside the `if containsNL(v)` branch.

#### Denylist

| Category | Keys |
|---|---|
| Docker daemon | `DOCKER_*` prefix (any Docker client credential) |
| Host context | `HOME`, `XDG_CONFIG_HOME`, `PATH`, `TMPDIR` |
| Network | `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `ALL_PROXY` |
| TLS | `SSL_CERT_FILE`, `SSL_CERT_DIR` |

Matching is case-insensitive. `DOCKER_` is a prefix match; others are exact.
The key is neither logged nor returned in the error; the error message
says only `denylisted multi-line env key "K"` to avoid leaking host-secret
names that an attacker might probe.

#### Tests

- `TestCliEnvDeny`: all denylisted keys (including `DOCKER_*` prefix and
  lower-case) return `true`.
- `TestCliEnvNotDenied`: `GITHUB_TOKEN`, `API_KEY`, non-denylisted keys
   return `false`.
- `TestWriteEnvFile_DenylistKeys`: four sub-cases:
   * single-line denylisted keys pass through the env file ✅
   * multi-line denylisted keys return an error ✅
   * lower-case multi-line denylisted keys return an error ✅
   * allowed multi-line key (`GITHUB_TOKEN`) flows through `multiLineEnv` ✅

### Implementation

- `writeEnvFile` now returns `(envFilePath string, multiLineEnv []string,
  cleanup func(), err error)`. It routes single-line values to the file and
  accumulates multi-line **keys** (not values) in `multiLineEnv`. Multi-line
  denylisted keys are rejected with an error.
- `dockerRunArgs` accepts `multiLineEnv []string` and appends `--env K`
  (key only) for each entry.
- `RunStep` appends `K=value` for each `K` in `multiLineEnv` to `cmd.Env`
   after `os.Environ()`.
- The `containsNL` helper is unchanged; it still rejects invalid keys
   with embedded newlines (C1/C1a file-injection defence).

### No schema or migration changes.

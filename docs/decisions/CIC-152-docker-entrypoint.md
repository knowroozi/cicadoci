# Decision record: CIC-152 — Docker executor ignores the image ENTRYPOINT

**Status:** In Review — PR (CIC-152)
**Opened:** 2026-10-03
**Parent:** runtime queue (CIC-152): `run:` steps break on any image that sets an
`ENTRYPOINT`, because the runner hands the step's command to the entrypoint as
trailing arguments instead of executing it.

## The question

A `run:` step is a shell command the operator wrote (e.g. `echo build`,
equivalent to the shell executor's `/bin/sh -c "…"`). The docker executor builds
`docker run --rm … <image> <cmd>…`. When the image sets an `ENTRYPOINT` (a common
shape — e.g. `ENTRYPOINT ["python"]`, `["sleep"]`, or a wrapper that starts a
server), docker executes the entrypoint and passes `<cmd>…` as *its* arguments,
so the step's command is never run and the step silently fails.

## Decision

Force `--entrypoint ""` on every container run. The empty string disables the
image's `ENTRYPOINT`, so docker execs the step's command directly — which is
exactly what the shell executor does (it runs `step.Command[0]` with the rest
of `step.Command`), keeping the two executors behavioral-equivalent.

This is a one-line addition to `dockerRunArgs`:

```go
args = append(args, "--entrypoint", "")
```

placed between the environment flags and the image. The step's `Command` is
already the full argv the shell executor executes, so no reformatting of the
command is needed — disabling the entrypoint is sufficient.

## Why this and not the alternatives

- **Force `--entrypoint /bin/sh` (then run the script as an arg):** wrong shape.
  `step.Command` is already a full argv that the shell executor runs directly
  (e.g. `["/bin/sh","-c","echo hi"]`); prepending another `/bin/sh` would double-wrap it.
- **Document the limitation and tell operators to avoid ENTRYPOINT images:** this
  is exactly the class of images operators reach for (official language toolchain
  images). A silent, data-dependent failure here is the worst outcome; disabling
  the entrypoint is a strict superset of "no entrypoint" and costs nothing for
  images that had none.
- **Run the container as a foreground server and shell in:** over-engineered;
  a `run:` step is a one-shot command, not a service, so a plain `docker run`
  with the entrypoint disabled is the right model.

## Consequences

- Every `docker`-executor step now runs its command regardless of the image's
   `ENTRYPOINT`, matching the shell executor's semantics.
- The test double `fakeDockerScript` gains a `--entrypoint` arm (`shift 2`, like
   the other value flags) and `TestDockerRunArgsDisablesImageEntrypoint` pins
   that `--entrypoint ""` precedes the image, which precedes the command.
- Images that relied on their `ENTRYPOINT` to provide the command must instead be
   invoked via a plain image + `run: <cmd>` in the workflow — the expected shape
  for one-shot build steps. No production image in the current e2e set depends on
  an `ENTRYPOINT`, so this is backward compatible in practice.

## Related

- `dockerReal_test.go` exercises the same plumbing against a real daemon when one
   is available (skipped otherwise); the regression here is the args-level pin.
- The shell executor (`Shell.RunStep`) is the behavioral reference this fix
   aligns the docker executor to.

## CIC-74 — Log-redaction wiring: fix `NoopMasker` default + add registration call

**Date:** 2026-09-28

**Problem:** Two defects in the log-redaction path:
1. `runnersvc.DefaultConfig()` set `Masker: redact.NoopMasker{}`, meaning
   even if `RegisterWithVariants` were called, a `NoopMasker.Scrub` is a
   no-op — the token would survive in logs regardless.
2. `RegisterWithVariants` / `Register` existed in `server/redact/masker.go`
   but had **zero non-test call sites**. Newly-minted GitHub installation
   tokens were never registered, so any log line that included a clone URL
   (e.g. a git-failed error `fatal: could not read Username ...
   https://x-access-token:TOKEN@gitHub.com/...`) leaked the token in raw
   log files and cloud log pipelines — violating the project's core
   "secrets never in logs" invariant.

**Decision:**
1. Change `DefaultConfig()` to return `redact.NewRegisteredMasker()` (a
   live, empty masker) instead of `NoopMasker{}`. The nil-guard
   `ensureNonNilMasker` is unchanged: it only replaces `nil`, which
   `DefaultConfig` never produces.
2. Add a `RegisteringMasker` sub-interface to `redact/masker.go`:
   ```go
   type RegisteringMasker interface {
        Masker
        Register(raw string)
        RegisterWithVariants(raw string)
   }
   ```
3. Add `Masker redact.RegisteringMasker` to `InstallationTokenMinter`.
   In `Mint()`, after extracting and validating `out.Token` but before
   returning, call `m.Masker.RegisterWithVariants(token)` if
   `m.Masker != nil`. Registration is nil-guarded, so existing call
   sites without a masker are unaffected.
4. New tests `TestMint_RegistersTokenWithMasker` and
   `TestMint_NilMaskerDoesNotPanic` pin both paths.

The deeper wire-up — passing the server's masker to the minter at init —
belongs to CIC-71 (main.go wiring).

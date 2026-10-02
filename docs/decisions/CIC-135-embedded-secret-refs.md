# Decision: Embedded secret-template `${{ secrets.X }}` references (CIC-135)

CIC-135: a CIC-79-style secret reference embedded in a larger value
(e.g. a bearer header `Bearer ${{ secrets.API_TOKEN }}`) is passed through
as the literal text instead of being interpolated. A workflow step that wants
an auth header like

    Authorization: Bearer ${{ secrets.GITHUB_TOKEN }}

ends up sending the literal string to the API instead of `Bearer <token>`, and
the job fails with an "Invalid username or password" result.

## Option chosen: Option 1 — interpolate (GHA parity)

The fix follows GitHub Actions parity. `internal/secretexpr/secretexpr.go`:
- `secretTemplateRe` (`\$\{\{\s*secrets\.[A-Za-z0-9_-]+\s*\}\}`) matches a
  `secrets.NAME` subexpression in ANY position within a value.
- `Refs` / `NamesIn` collect every embedded occurrence so the runner's
  `GetJobSecrets` still authorizes them.
- `Resolve` substitutes in place and preserves surrounding text; an unresolved
  reference (name not in the delivered map) is left as inert literal text — not
  recursively re-substituted and not a parse-time error (Option 1).
- `IsRef` reports `true` for a value containing a reference anywhere, which the
  fork `scrubSecrets` uses to drop any entry that carries a reference.
- `runner/secrets.go` `unresolvedSecrets(raw, secrets)`: the runtime
  "missing/unavailable secret" check runs `NamesIn` over the PRE-resolution raw
  env and subtracts the names present in `secrets`, so a delivered secret whose
  VALUE itself contains a `secrets.` template or bare reference is never falsely
  flagged, and no part of a secret value is re-parsed or echoed into
  `StatusUpdate.Detail` or the runner log (neither of which goes through the log
  masker — a content-leak vector; the blocking finding on the #108 review).
  `runner/job.go` computes `mergeEnv(spec, step)` once and feeds it to both
  `Resolve` and the check.
- `runner/job.go` `mergeEnv(spec.GetEnv(), step.GetEnv())` is computed once and
  shared between `Resolve` and `unresolvedSecrets`, so they observe the exact
  same raw inputs.

## Padding behaviour (Dwight #108 minor #1)

A whole-value template wrapped in spaces, e.g. `"     ${\{{ secrets.PAD }}     "`,
now resolves IN PLACE and preserves the surrounding spaces (the padding stays:
the value is `"     <secret-value>     "`), matching GitHub Actions. `TestResolve_
TemplateForm` asserts the padded result explicitly so the behaviour is pinned
rather than silently absorbed.

## Test literals

The embedded template literals in the tests are assembled by string
concatenation (e.g. `"pre-" + "${{ secrets.TOK }}-post"`) purely as a source
hygiene precaution: a naive textual pre-processor might otherwise corrupt the
`$`-adjacent-to-`\{\{` token into a `$ {{ ... }}` that the parser rejects.
Go string literals are never shell-expanded, so the concatenation carries no
runtime effect on the value — it only affects how the source file is read by
tooling.

## Option noted: Option 2 — reject at parse (`invalid_template`)

The alternative is to reject a reference that is not a bare whole-value at parse
time (a dedicated `invalid_template` error, like CIC-109 does for duplicate
keys). That keeps the parser conservative, but breaks the GHA-parity case the
ticket is about, so Option 1 is chosen.

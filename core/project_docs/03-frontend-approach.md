# Frontend Approach — No-Build, Server-Rendered, Live

Status: Accepted (see ADR-003) · Supersedes the earlier React/TypeScript recommendation

## Goal

Optimize for speed (perceived and actual), zero frontend compilation, and shipping the entire product — UI included — as one Go binary.

## Decision

**Server-rendered HTML (Go `html/template`) + SSE for live data + htmx (vendored, no build) + small hand-written vanilla JS islands.** Assets embedded via `embed.FS`.

## Why this fits this product

- A CI dashboard is overwhelmingly **server-state display**: run lists, statuses, logs, queue depth, forms. HTML-over-the-wire is the natural architecture (proven by Phoenix LiveView, Hotwire/Turbo, HTMX at scale).
- **No build step is an ops feature**, not just a dev convenience: `go build` produces the whole product; contributors need no Node toolchain; self-hosters get zero frontend-related failure modes.
- First paint is server-rendered HTML — fast on any device, no hydration cost.

## Transport: SSE by default, not WebSockets

The original instinct was "HTML over WSS." Adjusted after weighing:

| | SSE | WebSocket |
|---|---|---|
| Direction | Server → client (fits ~95% of our traffic: log lines, status changes) | Bidirectional |
| Reconnect | **Automatic**, with `Last-Event-ID` resume built into the browser | Hand-rolled reconnection + state resync |
| Infra | Plain HTTP; ALB/proxy friendly | ALB idle-timeout tuning, sticky-session concerns, upgrade handling |
| Client writes | Ordinary `fetch()` POSTs (simple, cacheable, testable) | Custom message protocol |

Rule: **SSE + fetch POSTs everywhere**; WebSockets reserved for genuinely bidirectional features when they arrive (interactive debug shell into a runner is the anticipated case).

## "Vanilla" — calibrated

Truly framework-free vanilla (`document.createElement` for everything) slowly reinvents a worse htmx. Instead:

- **htmx** (~14KB, single file, vendored into the repo — no npm, no compiler): declarative partial-page swaps, form handling, and its SSE extension for live regions. Datastar is the noted alternative if we want signals-style reactivity; evaluate once, pick one, don't churn.
- **Vanilla JS islands** (plain ES modules, no bundler) only where rich behavior is genuinely needed:
  1. **Log viewer** — virtualized scrolling (CI logs reach hundreds of MB; DOM-appending every line will die), ANSI color rendering, follow-tail toggle, search
  2. **Pipeline DAG view** — SVG, hand-rolled or a vendored micro-lib
  3. Command palette (later)
- ES modules load natively (`<script type="module">`); browsers we target (evergreen) need no transpilation. If we ever want types, JSDoc + `tsc --checkJs` in CI gives type-checking with **zero build output**.

## Server-side rendering mechanics

- `html/template` with a small layout/partial convention; template FS embedded, with a dev-mode flag to read from disk for live-reload.
- htmx endpoints return **HTML fragments** (a run row, a step block); full pages and fragments share partials.
- SSE endpoints emit either data events consumed by islands (log lines as text) or htmx-swappable HTML fragments (status badge updates).
- We do NOT adopt `templ` or any Go template compiler — that would reintroduce a codegen step; stdlib templates suffice at this UI's complexity.

## Trade-offs accepted

- Complex client-side interactivity is more work than in React — accepted, because the two rich components (logs, DAG) are bounded and worth hand-crafting.
- No component ecosystem — accepted; vendor micro-libs deliberately, one by one.
- Hiring/contributor familiarity skews React — mitigated by the approach being radically simple to learn (it's HTML).

## Revisit triggers

- If the workflow editor evolves into a rich visual builder, reassess (that single surface could become a compiled island without changing the rest).
- If SSE fan-out at scale strains the single-process model, introduce a dedicated fan-out layer before reaching for a frontend rewrite.

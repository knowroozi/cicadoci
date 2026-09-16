# ADR-003: No-build frontend — server-rendered HTML + SSE + htmx/vanilla islands

Status: Accepted · 2026-09-07 · Supersedes earlier React/TS recommendation

## Context
Goals: speed, zero frontend compilation, whole product in one Go binary. Original idea: vanilla JS + HTML over WSS.

## Decision
Go html/template server rendering, assets via embed.FS, htmx vendored (no npm/bundler), SSE (not WebSockets) for live data with Last-Event-ID resume, hand-written vanilla ES-module islands for the log viewer (virtualized) and DAG view. WebSockets deferred until a truly bidirectional feature (runner debug shell).

## Rationale
- CI UI is mostly server-state display; HTML-over-the-wire fits (LiveView/Hotwire/HTMX precedent).
- SSE beats WSS for one-way streams: free reconnect/resume, proxy/ALB friendly; writes are plain fetch POSTs.
- Pure hand-rolled vanilla reinvents a worse htmx; a vendored 14KB file keeps "no build" intact.

## Consequences
- go build produces the entire product; contributors need no Node.
- Rich interactivity is costlier; bounded to two islands.
- Revisit if a visual workflow builder materializes.

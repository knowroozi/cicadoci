## CIC-76 — MCP JSON-RPC transport deferred

**Date:** 2026-09-28

**Situation:** `server/mcp/server.go` exposes read-only diagnostic tools
as plain Go methods on `Server`:
```go
func (s *Server) ListRuns(ctx, principal, in) (runs, hasMore, err)
func (s *Server) GetRun(ctx, principal, runID) (run, err)
// ... 8 tools total
```

The real MCP (Model Context Protocol) requires a JSON-RPC 2.0 transport
over stdio, HTTP, or SSE with the `initialize` / `initialized` /
`tools/list` / `tools/call` lifecycle. Without it, no external MCP
client (Claude, Cursor, Aider, etc.) can connect to CICaDo.

**Scope of the gap:**
- No `initialize` handshake — the client cannot negotiate capabilities.
- No `tools/list` response — the client cannot discover available tools.
- No `tools/call` dispatch — the client cannot invoke a tool.
- No JSON-RPC 2.0 framing — no `id` / `result` / `error` envelope.
- No stdio transport — no way for an agent to spawn the server as
    a subprocess and communicate via stdin/stdout.

**Decision:** Defer M9 to add the full MCP transport.

Rationale:
1. The Go-method layer is complete and tested. Every tool is
    functional — `ListRuns`, `GetRun`, `ClassifyFailure`,
    `ValidateWorkflow` all have real implementations.
2. The `mcpauth.Authenticator` (CIC-54) is wired for the
    HTTP-transport case (Bearer token, OAuth2, etc.).
3. The MCP protocol spec is stable and well-documented (1.0,
    April 2025). Implementing it is a focused task with no
    architectural ambiguity.
4. Landing it in M9 (alongside the remaining cloud infrastructure)
    keeps the "make every subsystem real" work in one milestone
    rather than scattered across M1–M8.

**Interim state:** MCP tool handlers exist as Go methods.
No external agent can connect today. The code is ready to wire
behind a JSON-RPC transport layer when the decision to add it is
made (likely alongside M5's OIDC/token infrastructure).

**No code changes in this PR** — a re-scope decision, not a fix.

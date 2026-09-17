# web

Server-rendered UI assets, served from the server binary via `embed.FS`.

Arrives in M6. There is deliberately **no build step**: Go `html/template`,
vendored htmx, SSE, and small vanilla ES-module islands. No npm, no bundler, no
React, no templ — `go build` produces the whole product. See
[ADR-003](../project-docs/adr/ADR-003-frontend-no-build.md) and
[the frontend approach](../project-docs/03-frontend-approach.md).

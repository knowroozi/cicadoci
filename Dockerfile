# Both binaries from one multi-stage build. The server image carries the web UI
# too, once there is one: embed.FS means there is never a second asset artifact
# to ship or serve (ADR-003).
#
# Build a specific binary with:
#   docker build --target server .
#   docker build --target runner .

# ─── build ───────────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so editing source does not invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off gives a static binary that runs on any base image, including scratch.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
        -o /out/cicado-server ./server/cmd/cicado-server && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
        -o /out/cicado-runner ./runner/cmd/cicado-runner

# ─── server ──────────────────────────────────────────────────────────────────
# The control plane runs no user code, so it gets a minimal base and a
# non-root user.
FROM alpine:3.21 AS server

RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 cicado

COPY --from=build /out/cicado-server /usr/local/bin/cicado-server

USER cicado
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/cicado-server"]

# ─── runner ──────────────────────────────────────────────────────────────────
# The runner deliberately carries more than the server: it executes job steps,
# and the shell executor runs them against this filesystem. git and a shell are
# the minimum that makes it useful.
#
# It runs as root here because the development stack's shell executor needs to
# write its workspace. Production runners should not: see docs/development.md.
FROM alpine:3.21 AS runner

RUN apk add --no-cache ca-certificates tzdata git bash coreutils && \
    mkdir -p /var/lib/cicado/workspaces

COPY --from=build /out/cicado-runner /usr/local/bin/cicado-runner

# No USER directive here, deliberately — this stage runs as root. See the
# comment above the FROM line.
ENTRYPOINT ["/usr/local/bin/cicado-runner"]

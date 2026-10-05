# CICaDo — multi-stage Docker image
#
# Two purpose-built runtime targets:
#
#   docker build --target server  → distroless/static:nonroot, minimal attack surface,
#                                    no shell — for the API/web server only
#
#   docker build --target runner  → debian:12-slim with git + shell, non-root,
#                                    the runtime the shell executor actually needs
#
# The runner deliberately carries more than the server: it executes job steps,
# and the shell executor runs them against this filesystem. git and a shell are
# the minimum that makes it useful. It runs as a non-root user here; root is
# only needed when the development shell-executor writes its workspace outside
# a mounted volume.
#
# Usage:
#   make docker-build           # both images
#   docker build --target server -t cicado-server:latest .
#   docker build --target runner -t cicado-runner:latest .

## Build stage — all three binaries, statically linked, CGO disabled
# go.mod requires go >= 1.26.0 (the `go` directive) with GOTOOLCHAIN=local, so
# the build toolchain MUST be >= 1.26; a 1.23 image fails `go mod download`
# (CIC-134: the only failing CI job was 'Docker images'). 1.26 matches the
# repo's .github/workflows/ci.yml GO_VERSION.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" \
    -o /bin/cicado-server ./server/cmd/cicado-server
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" \
    -o /bin/cicado-runner ./runner/cmd/cicado-runner
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" \
    -o /bin/cicado ./cmd/cicado

## Target: server — distroless/static, no shell, minimal attack surface
FROM gcr.io/distroless/static:nonroot AS server
COPY --from=build /bin/cicado-server /cicado-server
EXPOSE 8080 9090
ENTRYPOINT ["/cicado-server"]

## Target: runner — debian-slim, carries git + shell for the executor
# The shell executor (runner/executor/shell.go) invokes sh -c for each job
# step. distroless/static has no shell at all, so the runner image must use
# a base that ships a shell. git is required for git clone to
# actually clone a repo. debian:12-slim is the smallest Debian image with
# both, and it can run as non-root.
FROM debian:12-slim AS runner
RUN apt-get update && \
    apt-get install -y --no-install-recommends git ca-certificates && \
    rm -rf /var/lib/apt/lists/*
# Non-root user (UID 65532 — matches gcloud non-root convention)
RUN useradd --uid 65532 --shell /bin/bash --create-home cicado
USER cicado
WORKDIR /home/cicado
COPY --from=build /bin/cicado-runner /home/cicado/cicado-runner
COPY --from=build /bin/cicado /home/cicado/cicado
EXPOSE 9090
ENTRYPOINT ["/home/cicado/cicado-runner"]

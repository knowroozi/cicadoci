#!/usr/bin/env bash
# build.sh — build CICaDo binaries for Docker packaging
#
# Usage: ./build.sh [arch] [os]
# Example: ./build.sh amd64 linux
set -euo pipefail

ARCH="${1:-amd64}"
OS="${2:-linux}"
OUT="dist"
mkdir -p "$OUT"

echo "==> Building cicado-server for ${OS}/${ARCH}..."
CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" go build -ldflags="-s -w" \
   -o "$OUT/cicado-server" ./server/cmd/cicado-server

echo "==> Building cicado-runner for ${OS}/${ARCH}..."
CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" go build -ldflags="-s -w" \
   -o "$OUT/cicado-runner" ./runner/cmd/cicado-runner

echo "==> Building cicado CLI for ${OS}/${ARCH}..."
CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" go build -ldflags="-s -w" \
   -o "$OUT/cicado" ./cmd/cicado

echo "==> Build complete: ${OUT}/"
ls -lh "$OUT/"
echo "==> Done."

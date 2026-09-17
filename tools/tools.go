//go:build tools

// Package tools pins the build-time tooling this repo uses.
//
// It lives in its own module on purpose. Pinning buf in the root module drags
// roughly 130 indirect dependencies — containerd, docker, quic-go, otel, zap —
// into the go.mod of a project whose entire pitch is a small dependency
// surface, and into every dependency audit run against it. The root module's
// requirements should describe what the shipped binaries actually link.
//
// `make dev-deps` installs these into ./bin from the versions pinned here.
package tools

import (
	_ "github.com/bufbuild/buf/cmd/buf"
	_ "github.com/golangci/golangci-lint/v2/cmd/golangci-lint"
	_ "google.golang.org/grpc/cmd/protoc-gen-go-grpc"
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"
)

# proto

Two subtrees here that are easy to mix up on first glance:

- **`cicado/runner/v1/runner.proto`** — the source. Frozen (ADR-004): additive
  changes only. Package `cicado.runner.v1`, matching the path.
- **`runner/v1/`** — the generated Go (`runner.pb.go`, `runner_grpc.pb.go`).
  Committed, not gitignored; regenerate with `make proto`, and
  `make generate-check` fails CI if what's committed doesn't match.

They don't share a path because the generated code's location comes from the
proto's own `option go_package = "cicadoci.com/cicado/proto/runner/v1;runnerv1"`
— that's the wire contract's own declaration of where its Go bindings live,
stripped of the module prefix (see `buf.gen.yaml`). The source file's location
is a separate decision: it mirrors the proto *package* path
(`cicado/runner/v1`), which is the convention buf itself expects. The two
happen to diverge here because the package name (`cicado`) and the Go import
path's directory (`cicado.com/cicado` → `cicado`... `runner`) aren't spelled
identically. Renaming either would mean either moving the frozen file's
canonical path or diverging from the Go ecosystem's `go_package` convention —
neither seemed worth it for what is otherwise just a one-directory offset.

If you're looking for the generated code: it's under `runner/`, not under
`cicado/`.

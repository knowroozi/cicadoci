## CIC-73 — S3 Presigned-URL provider deferred to M9

**Date:** 2026-09-28

**Situation:** `PreSignedURLProvider` is defined as an interface in
`server/artifacts/artifacts.go`. The only implementation is
`MemoryPreSigner` in `server/artifacts/memory.go`, which returns
`prefix + "/" + key` — suitable for tests only. No S3 driver exists.

The interface is correct:
```go
type PreSignedURLProvider interface {
     UploadURL(ctx, key, contentType, maxBytes, expiry) (url, err)
    DownloadURL(ctx, key, expiry) (url, err)
}
```

**Decision:** Defer the S3 driver to M9 (Docker, Lambda, Terraform, S3).

The S3 implementation requires:
- `aws-sdk-go-v2` (adding an external dependency)
- AWS credentials (from environment, IAM role, or OIDC — see CIC-39)
- Bucket name / region config

The M1–M8 milestones can proceed without a production S3 driver by
using `MemoryPreSigner` for integration tests and the real driver
landed as part of M9. The M9 milestone (Terraform/AMI/cloud-init)
already includes S3 bucket creation, so landing the driver there
co-locates the code with the infrastructure it depends on.

**Re-scope note:** This is *not* a code fix — it is a decision
document for the ticket, making the deferral explicit so the
`MemoryPreSigner`-only state is a conscious choice, not a forgotten
implementation.

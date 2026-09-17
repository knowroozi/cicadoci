# terraform

The AWS reference deployment module. Arrives in M9.

Planned shape (see [architecture §9](../project-docs/02-architecture.md)):
ECS Fargate for the control plane behind an ALB, RDS Postgres, S3 for logs and
artifacts, KMS for secret envelope encryption, an IAM OIDC provider for keyless
job identity, and an EC2 ASG for runners.

Runners are on EC2 rather than Fargate because Fargate cannot run runners that
build images — no privileged mode, no Docker-in-Docker. The kaniko and
rootless-BuildKit paths are documented for Fargate-only shops.

The MVP gate on this module is `terraform apply` from a clean AWS account to a
working install in under 30 minutes.

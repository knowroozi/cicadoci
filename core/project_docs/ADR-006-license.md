# ADR-006: Apache-2.0 core license

Status: Accepted · 2026-09-07

## Context
Drone's relicensing (Apache -> source-available) ruptured community trust and produced the Woodpecker fork. Earthly's shutdown shows CI monetization is hard and OSS cannibalizes paid compute.

## Decision
Core platform is Apache-2.0, permanently. If monetization ever happens: clearly-delineated separate enterprise add-ons (SSO/SAML, advanced RBAC, compliance/audit exports, managed control plane) — never relicensing the core, stated publicly from day one.

## Consequences
- Maximizes adoption and contributor trust; forecloses rug-pull monetization.
- Revenue (if pursued) must come from operational/compliance value, not core CI compute.

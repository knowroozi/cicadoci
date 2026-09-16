# ADR-007: Product name — CICaDo

Status: Accepted (pending final registration checklist below) · 2026-09-07

## Decision

The product is named **CICaDo** (stylized so the CI/CD letters are prominent in the mark; plain-text spelling "Cicado"). Cicada mascot/branding potential is retained. "Do" carries the value prop: it *does* your CI/CD.

**Registered identity (2026-09-16):**
- Primary domain / product site: **cicadoci.com** (purchased)
- Hosted web app: **cicado.dev** (purchased)
- GitHub org: **cicadoci**
- Social handles: **cicadoci** (X, Bluesky, etc.)

## Context — how we got here

1. **Original candidate: CICaDa.** Rejected after collision research found the name already used *twice for this exact product category*, plus live adjacent trademarks:
   - [cicada.build / cicadahq](https://github.com/cicadahq/cicada) — "Write CI/CD pipelines in TypeScript" (Fig founders, launched 2023, archived Sept 2023; domain still registered)
   - [Cicada Software, Inc.](https://github.com/Cicada-Software) — "a FOSS alternative to GitHub Actions and GitLab CI" (incorporated; cicada.sh)
   - Live/pending CICADA marks in Class 9 software: [Cicada Technologies (fintech trading platform)](https://uspto.report/TM/90579116) and a CACI International filing
   - SEO burden: "cicada CI" surfaces the dead products + the Cicada 3301 puzzle; good GitHub orgs taken
   - Note: the CICaDa capitalization does not differentiate legally — confusion analysis ignores stylization.
2. **Alternates evaluated and rejected** (all collided in software/dev tools): Cinch ([cinch.io](https://cinch.io), [cinchsoftware.io](https://cinchsoftware.io), [cinchcli.com](https://cinchcli.com) — a dev CLI that aliases `ci`, github.com/cinch taken), Katydid ([active validation-language toolkit owning the org](https://github.com/katydid)), Magicicada ([Ubuntu One fork](https://github.com/chicharreros/magicicada-server) + claimed username), CadenCI (Cadence Design Systems / Uber Cadence adjacency). Pattern observed: every real English word and even the Latin genus is occupied; **coined names are where the clean space is** — and fanciful marks get the strongest trademark protection.
3. **CICaDo clearance findings (2026-09-07):**
   - Dev/software space: **clean** — no product, tool, repo, company, or npm package named "cicado" found (npm has only `cicada-*` packages)
   - Only web presences: a My Singing Monsters game decoration ("Pipes of Cicado"), a small design studio Instagram (@cicadostudio), a Pinterest user
   - No CICADO trademark filings found at USPTO
   - **`cicado.io` is taken — by Cicada Technologies**, the fintech holding the pending CICADA Class-9 mark. Different goods/services field (institutional bond trading vs developer CI/CD), but this is the one adjacency professional clearance must evaluate.

## Registration checklist

- [x] Domains: **cicadoci.com** (primary/site) + **cicado.dev** (hosted app) — purchased 2026-09-16 (`.io` is taken by Cicada Technologies; `.da` is not a TLD — two-letter TLDs are ISO country codes only)
- [x] GitHub org: `github.com/cicadoci`
- [x] Social handles: **cicadoci** (X, Bluesky; add Mastodon/fosstodon + LinkedIn page when creating those presences)
- [x] npm scope: `@cicadoci` (match the org handle) — secured 2026-09-16
- [ ] Docker Hub namespace: `cicadoci`
- [ ] Homebrew formula name (`cicado`) / crates.io / Go module vanity path (e.g. `cicadoci.com/cicado`)
- [ ] USPTO TESS knockout search: "cicado" + sound-alikes (cicada, cikada, cycada) in Classes 9 & 42
- [ ] Before public launch: ~1 hour IP-counsel review, specifically re: Cicada Technologies' CICADA mark + their cicado.io domain

## Consequences

- All docs/repos use **CICaDo** in marks/branding, **Cicado**/`cicado` in prose, packages, binaries, and paths (binary: `cicado`, runner: `cicado-runner`).
- Web presence: **cicadoci.com** is the product/marketing/docs site; **cicado.dev** is the hosted web app. The GitHub org and social handles are **cicadoci** — package namespaces (npm/Docker Hub) should match `cicadoci` for consistency.
- Supersedes the "Forgelight" working codename.
- If the counsel review surfaces a blocking conflict, fallback candidate is **CICaDia** (same clearance process applies).

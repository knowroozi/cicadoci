# Linear Done Log

This script records every Linear ticket that reaches a **Done** state into an append‑only JSON log (`docs/linear-done-log.json`).

## How it works
1. It queries Linear's GraphQL API (`https://api.linear.app/graphql`) using the `LINEAR_API_KEY` secret. The query fetches all issues where `state.type == "completed"` with cursor‑based pagination (100 items per request).
2. Each issue is mapped to the required fields: identifier, title, description, state, completedAt, createdAt, sorted labels, priority, assignee, project/milestone, URL, and attached PR links.
3. The script reads the existing log, merges new tickets while deduping by `identifier`, sorts deterministically by `completedAt` then `identifier`, and writes the updated log with a trailing newline.
4. The GitHub Actions workflow runs this script daily (and on manual dispatch) and commits the log only if `docs/linear-done-log.json` changed.

## Running locally
```sh
go run ./scripts/linear-done-log/main.go
```
Ensure the environment variable `LINEAR_API_KEY` is set; the script will exit with an error if it is missing.



## Setup

Repository setting *Allow GitHub Actions to create and approve pull requests* must be enabled, and the `LINEAR_API_KEY` secret must exist.

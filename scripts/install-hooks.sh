#!/usr/bin/env bash
# Install CICaDo git hooks by pointing this repo's core.hooksPath at ./.githooks.
#
# `git config` lives per-worktree, not in the tree itself, so a fresh clone never
# gets the hook "for free" just from checking out .githooks/ — it must be wired up
# once locally via this script (or by hand: `git config core.hooksPath .githooks`).
# That is deliberate: we avoid silently mutating ~/.config or a developer's global git profile.

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "${repo_root}"

if [[ ! -f ".githooks/pre-commit" ]]; then
	echo "install-hooks: .githooks/pre-commit not found; is this the CICaDo repo root?" >&2
	exit 1
fi

git config core.hooksPath .githooks
chmod +x .githooks/pre-commit

echo "install-hooks: set 'core.hooksPath' to '.githooks' for this worktree"
echo "   pre-commit hook will run golangci-lint (via 'make lint') before each commit."
echo
echo "Per-clone note, not a repo-wide change: core.hooksPath is stored in this worktree's .git/config,"
echo "so it does not travel with a clone. Run 'scripts/install-hooks.sh' again on any new local checkout."

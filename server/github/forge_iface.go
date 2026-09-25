package github

import "context"

// Forge is the capability interface that a code-hosting provider must
// implement for CICaDo to talk to it. CIC-28 will make this the sole
// seam between the server and any forge (GitHub, GitLab, Bitbucket).
//
// For now only GitHub implements it via ChecksReporter + Installer
// (CIC-26). The interface is intentionally minimal — just the three
// directions of work that the server needs:
//
//  1. Status reporting: prepare, start, complete a "check" for a run.
//  2. Rerun: accept a rerun request from the forge.
//  3. Clone credentials: for a given run, produce a short-lived token
//     that the runner can use to clone the repo (CIC-26).
//
// It is placed in the `github` package for now so that the concrete
// implementation has everything it needs. When CIC-28 lands it will
// move to `server/forge/` and this file will become the canonical
// location.
type Forge interface {
	// --- Status reporting (from CIC-25) ---

	// Prepare creates a check for a run in the "queued/waiting" state.
	Prepare(ctx context.Context, ref RunRef) (checkID int64, err error)

	// Start marks the check as in progress.
	Start(ctx context.Context, checkID int64, ref RunRef) error

	// Complete marks the check as done with a conclusion.
	Complete(ctx context.Context, checkID int64, ref RunRef,
		conclusion CheckConclusion, annotations []Annotation) error

	// Rerun handles a rerun request (from a "Rerun checks" button in
	// the forge UI); the forge tells CICaDo to re-run and the
	// implementation enqueues a new run.
	Rerun(ctx context.Context, ref RerunRef) (runID string, err error)

	// --- Clone credentials (from CIC-26; unimplemented for now) ---

	// CloneCredentialsForRun returns a token that, when embedded in the
	// git clone URL for a run's source ref, grants read-only access to
	// the repo for a limited time. It is never persisted and is
	// registered with the log-redaction masker before handoff.
	CloneCredentialsForRun(ctx context.Context, ref RunRef) (token string, err error)
}

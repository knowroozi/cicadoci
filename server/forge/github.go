package forge

import (
	"context"
	"errors"
)

// Reporter is the subset of a checks API client that Forge needs.
// It is defined here so the forge package doesn't import server/github
// (which would be a cycle). Go interface satisfaction is nominal, not
// structural: github.ChecksClient does NOT satisfy this interface on
// its own, since github.RunRef/RerunRef/CheckConclusion/Annotation are
// distinct named types from their forge counterparts here, even though
// their fields line up. server/github's ChecksReporterAdapter wraps a
// *ChecksClient and translates between the two type sets to actually
// satisfy Reporter — server/github is free to import this leaf package
// without creating a cycle.
type Reporter interface {
	Prepare(ctx context.Context, ref RunRef) (checkID int64, err error)
	Start(ctx context.Context, checkID int64, ref RunRef) error
	Complete(ctx context.Context, checkID int64, ref RunRef,
		conclusion Conclusion, annotations []Annotation) error
	Rerun(ctx context.Context, ref RerunRef) (runID string, err error)
}

// TokenMinter produces a short-lived token for a git clone operation.
// A concrete minter needs ctx (minting is a network call) and its own
// way of resolving ref.Org/ref.Repo to an installation ID; see
// server/github's InstallationTokenMinterAdapter for the GitHub driver.
type TokenMinter interface {
	// CloneCredentialsForRun returns a token for the given run.
	CloneCredentialsForRun(ctx context.Context, ref RunRef) (token string, err error)
}

// githubForge is a Forge backed by a Reporter (status reporting) and
// a TokenMinter (clone creds).
type githubForge struct {
	name string
	rep  Reporter
	mint TokenMinter
}

// NewGitHubForge creates a Forge backed by the given Reporter.
// When mint is nil, CloneCredentialsForRun returns an error.
func NewGitHubForge(rep Reporter, mint TokenMinter) *githubForge {
	return &githubForge{name: "github", rep: rep, mint: mint}
}

// Name returns "github".
func (g *githubForge) Name() string { return g.name }

// Prepare delegates to the reporter.
func (g *githubForge) Prepare(ctx context.Context, ref RunRef) (int64, error) {
	return g.rep.Prepare(ctx, ref)
}

// Start delegates to the reporter.
func (g *githubForge) Start(ctx context.Context, checkID int64, ref RunRef) error {
	return g.rep.Start(ctx, checkID, ref)
}

// Complete delegates to the reporter.
func (g *githubForge) Complete(ctx context.Context, checkID int64, ref RunRef,
	conclusion Conclusion, annotations []Annotation,
) error {
	return g.rep.Complete(ctx, checkID, ref, conclusion, annotations)
}

// Rerun delegates to the reporter.
func (g *githubForge) Rerun(ctx context.Context, ref RerunRef) (string, error) {
	return g.rep.Rerun(ctx, ref)
}

// CloneCredentialsForRun delegates to the minter.
func (g *githubForge) CloneCredentialsForRun(ctx context.Context, ref RunRef) (string, error) {
	if g.mint == nil {
		return "", errNoMinter
	}
	return g.mint.CloneCredentialsForRun(ctx, ref)
}

// errNoMinter is returned when CloneCredentialsForRun is called but no
// TokenMinter was provided.
var errNoMinter = errors.New("forge: no token minter configured") // sentinel; replaced at wiring time

var _ Forge = (*githubForge)(nil)

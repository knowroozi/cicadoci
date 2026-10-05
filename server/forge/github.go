package forge

import (
	"context"
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

// githubForge is a Forge backed by a Reporter (status reporting).
type githubForge struct {
	name string
	rep  Reporter
}

// NewGitHubForge creates a Forge backed by the given Reporter.
func NewGitHubForge(rep Reporter) *githubForge {
	return &githubForge{name: "github", rep: rep}
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

var _ Forge = (*githubForge)(nil)

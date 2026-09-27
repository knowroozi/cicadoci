package github

import (
	"context"
	"fmt"

	"cicadoci.com/cicado/server/forge"
)

// InstallationResolver maps an org/repo pair to the GitHub App
// installation ID that covers it. There is no static config for this
// yet (M3 wiring ticket) — callers construct one from whatever they use
// to track installations (a DB table, a static map for a single-org
// deployment, etc.).
type InstallationResolver func(org, repo string) (installationID int64, err error)

// ChecksReporterAdapter wraps a *ChecksClient so it satisfies
// forge.Reporter. Go interface satisfaction is nominal: even though
// RunRef/RerunRef/CheckConclusion/Annotation are field-identical to
// their forge counterparts, they are distinct named types, so a bare
// *ChecksClient does not implement forge.Reporter on its own. This
// adapter does the type translation.
type ChecksReporterAdapter struct {
	Client *ChecksClient
}

// NewChecksReporterAdapter wraps client as a forge.Reporter.
func NewChecksReporterAdapter(client *ChecksClient) *ChecksReporterAdapter {
	return &ChecksReporterAdapter{Client: client}
}

// Prepare delegates to the wrapped client.
func (a *ChecksReporterAdapter) Prepare(ctx context.Context, ref forge.RunRef) (int64, error) {
	return a.Client.Prepare(ctx, toRunRef(ref))
}

// Start delegates to the wrapped client.
func (a *ChecksReporterAdapter) Start(ctx context.Context, checkID int64, ref forge.RunRef) error {
	return a.Client.Start(ctx, checkID, toRunRef(ref))
}

// Complete delegates to the wrapped client.
func (a *ChecksReporterAdapter) Complete(ctx context.Context, checkID int64, ref forge.RunRef,
	conclusion forge.Conclusion, annotations []forge.Annotation,
) error {
	return a.Client.Complete(ctx, checkID, toRunRef(ref), toConclusion(conclusion), toAnnotations(annotations))
}

// Rerun delegates to the wrapped client.
func (a *ChecksReporterAdapter) Rerun(ctx context.Context, ref forge.RerunRef) (string, error) {
	return a.Client.Rerun(ctx, toRerunRef(ref))
}

// InstallationTokenMinterAdapter wraps an *InstallationTokenMinter so
// it satisfies forge.TokenMinter. A bare *InstallationTokenMinter
// can't: its Mint method needs an installation ID that forge.RunRef
// doesn't carry, so the adapter resolves one via Resolver.
type InstallationTokenMinterAdapter struct {
	Minter   *InstallationTokenMinter
	Resolver InstallationResolver
}

// NewInstallationTokenMinterAdapter wraps minter as a forge.TokenMinter,
// resolving each run's installation ID via resolve.
func NewInstallationTokenMinterAdapter(minter *InstallationTokenMinter, resolve InstallationResolver) *InstallationTokenMinterAdapter {
	return &InstallationTokenMinterAdapter{Minter: minter, Resolver: resolve}
}

// CloneCredentialsForRun resolves ref's installation ID and mints a
// clone-URL token for it.
func (a *InstallationTokenMinterAdapter) CloneCredentialsForRun(ctx context.Context, ref forge.RunRef) (string, error) {
	installationID, err := a.Resolver(ref.Org, ref.Repo)
	if err != nil {
		return "", fmt.Errorf("resolve installation id for %s/%s: %w", ref.Org, ref.Repo, err)
	}
	// client is reserved by CloneCredentialsForRunOnClient for future
	// wiring; the adapter has no ChecksClient of its own to offer.
	return CloneCredentialsForRunOnClient(ctx, nil, a.Minter, installationID, toRunRef(ref))
}

func toRunRef(ref forge.RunRef) RunRef {
	return RunRef{
		Org:        ref.Org,
		Repo:       ref.Repo,
		Sha:        ref.Sha,
		RunID:      ref.RunID,
		Branch:     ref.Branch,
		Started:    ref.Started,
		Finished:   ref.Finished,
		DetailsURL: ref.DetailsURL,
	}
}

func toRerunRef(ref forge.RerunRef) RerunRef {
	return RerunRef{
		Org:       ref.Org,
		Repo:      ref.Repo,
		Sha:       ref.Sha,
		Branch:    ref.Branch,
		CheckName: ref.CheckName,
	}
}

func toConclusion(c forge.Conclusion) CheckConclusion {
	return CheckConclusion(c)
}

func toAnnotations(anns []forge.Annotation) []Annotation {
	if anns == nil {
		return nil
	}
	out := make([]Annotation, len(anns))
	for i, a := range anns {
		out[i] = Annotation{
			Path:      a.Path,
			Line:      a.Line,
			Column:    a.Column,
			EndLine:   a.EndLine,
			EndColumn: a.EndColumn,
			Anchor:    a.Anchor,
			Message:   a.Message,
			Title:     a.Title,
			Level:     a.Level,
		}
	}
	return out
}

var (
	_ forge.Reporter    = (*ChecksReporterAdapter)(nil)
	_ forge.TokenMinter = (*InstallationTokenMinterAdapter)(nil)
)

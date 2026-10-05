package github

import (
	"context"

	"cicadoci.com/cicado/server/forge"
)

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

var _ forge.Reporter = (*ChecksReporterAdapter)(nil)

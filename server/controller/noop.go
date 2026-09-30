// CIC-71d: NoOpForge
//
// NoOpForge satisfies forge.Forge but does nothing.  It is used in
// production main.go when the operator has not yet configured a
// real forge backend (no GitHub App webhook secret set, no installation
// token subsystem).
//
// The controller's report() function already tolerates a NoOp forge:
// Prepare returns 0/nil and nothing is logged.
//
// CIC-38/39/40 add the real githubForge with a ChecksReporter and
// TokenMinter.  At that point NoOpForge is no longer needed in main.go.
package controller

import (
	"context"

	"cicadoci.com/cicado/server/forge"
)

// NoOpForge is a no-op implementation of forge.Forge.
//
// It satisfies the interface but produces no side effects: no checks
// created, no annotations, no tokens.  Used as the default forge in
// controller wiring until a real GitHub forge is configured.
type NoOpForge struct{}

func (NoOpForge) Name() string { return "noop" }

func (NoOpForge) Prepare(_ context.Context, _ forge.RunRef) (int64, error) {
	return 0, nil
}

func (NoOpForge) Start(_ context.Context, _ int64, _ forge.RunRef) error {
	return nil
}

func (NoOpForge) Complete(_ context.Context, _ int64, _ forge.RunRef,
	_ forge.Conclusion, _ []forge.Annotation,
) error {
	return nil
}

func (NoOpForge) Rerun(_ context.Context, _ forge.RerunRef) (string, error) {
	return "", nil
}

func (NoOpForge) CloneCredentialsForRun(_ context.Context, _ forge.RunRef) (string, error) {
	return "", nil
}

var _ forge.Forge = NoOpForge{}

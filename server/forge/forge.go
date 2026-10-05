// Package forge defines the adapter interface between CICaDo and a
// code-hosting provider ("forge"). GitHub is the sole driver in MVP;
// GitLab/Bitbucket are later additions.
//
// The interface is intentionally minimal: three directions of work
// that the server needs from a forge:
//
//  1. Status reporting (checks/builds): prepare, start, complete.
//  2. Rerun: accept a "Rerun checks" request.
//  3. Clone credentials: produce a short-lived token for a git clone.
//
// This package is a leaf: it depends on nothing else in server/.
package forge

import (
	"context"
	"time"
)

// Forge is the capability a code-hosting provider must implement.
type Forge interface {
	// Name returns the forge's short identifier ("github", "gitlab").
	Name() string

	// Prepare creates a check/build for a run in the "queued" state.
	Prepare(ctx context.Context, ref RunRef) (checkID int64, err error)

	// Start marks the check as in progress.
	Start(ctx context.Context, checkID int64, ref RunRef) error

	// Complete marks the check as done with a conclusion.
	Complete(ctx context.Context, checkID int64, ref RunRef,
		conclusion Conclusion, annotations []Annotation) error

	// Rerun handles a "Rerun checks" request.
	Rerun(ctx context.Context, ref RerunRef) (runID string, err error)
}

// RunRef identifies a run for status reporting.
type RunRef struct {
	Org        string
	Repo       string
	Sha        string
	RunID      string
	Branch     string
	Started    *time.Time
	Finished   *time.Time
	DetailsURL string
}

// RerunRef identifies a check-rerun request.
type RerunRef struct {
	Org       string
	Repo      string
	Sha       string
	Branch    string
	CheckName string
}

// Conclusion is the terminal quality of a check/run.
type Conclusion string

const (
	ConclusionSuccess        Conclusion = "success"
	ConclusionFailure        Conclusion = "failure"
	ConclusionSkipped        Conclusion = "skipped"
	ConclusionNeutral        Conclusion = "neutral"
	ConclusionActionRequired Conclusion = "action_required"
	ConclusionCancelled      Conclusion = "cancelled"
)

// Annotation is a single annotation attached to a check.
type Annotation struct {
	Path      string
	Line      int
	Column    int
	EndLine   int
	EndColumn int
	Anchor    string
	Message   string
	Title     string
	Level     string // "warning", "failure", "error"
}

// Registry holds a set of Forge implementations keyed by name.
type Registry struct {
	forges map[string]Forge
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{forges: make(map[string]Forge)}
}

// Register adds a forge implementation. It panics on duplicate name.
func (r *Registry) Register(f Forge) {
	name := f.Name()
	if _, exists := r.forges[name]; exists {
		panic("forge: duplicate registration for " + name)
	}
	r.forges[name] = f
}

// Get returns the forge for a given name.
func (r *Registry) Get(name string) (Forge, bool) {
	f, ok := r.forges[name]
	return f, ok
}

// Names returns the registered forge names.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.forges))
	for name := range r.forges {
		names = append(names, name)
	}
	return names
}

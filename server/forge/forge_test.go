package forge

import (
	"context"
	"testing"
)

// stubForge is a no-op Forge for testing the Registry.
type stubForge struct {
	name string
	// Track calls.
	prepared  []RunRef
	completed []RunRef
}

func (s *stubForge) Name() string { return s.name }
func (s *stubForge) Prepare(_ context.Context, ref RunRef) (int64, error) {
	s.prepared = append(s.prepared, ref)
	return 1, nil
}
func (s *stubForge) Start(_ context.Context, _ int64, ref RunRef) error {
	// no-op
	_ = ref
	return nil
}
func (s *stubForge) Complete(_ context.Context, _ int64, ref RunRef, conclusion Conclusion, _ []Annotation) error {
	s.completed = append(s.completed, ref)
	// no-op
	_ = conclusion
	return nil
}
func (s *stubForge) Rerun(_ context.Context, ref RerunRef) (string, error) {
	// no-op
	_ = ref
	return "run-123", nil
}
func (s *stubForge) CloneCredentialsForRun(_ context.Context, ref RunRef) (string, error) {
	// no-op
	_ = ref
	return "stub-token", nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	reg := NewRegistry()
	p := &stubForge{name: "github"}
	reg.Register(p)

	got, ok := reg.Get("github")
	if !ok {
		t.Fatal("Get returned false for github")
	}
	if got != p {
		t.Fatal("Get returned wrong forge")
	}
}

func TestRegistry_DuplicatePanics(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubForge{name: "github"})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate")
		}
	}()
	reg.Register(&stubForge{name: "github"})
}

func TestRegistry_GetUnknown(t *testing.T) {
	reg := NewRegistry()
	_, ok := reg.Get("nonexistent")
	if ok {
		t.Fatal("Get(unknown) should return false")
	}
}

func TestRegistry_MultipleForges(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubForge{name: "github"})
	reg.Register(&stubForge{name: "gitlab"})
	if n := len(reg.Names()); n != 2 {
		t.Fatalf("Names() returns %d, want 2", n)
	}
}

func TestStubForge_PrepareCompletes(t *testing.T) {
	p := &stubForge{name: "test"}
	ref := RunRef{Org: "o", Repo: "r", Sha: "abc"}
	_, err := p.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Start(context.Background(), 1, ref)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Complete(context.Background(), 1, ref, ConclusionSuccess, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.prepared) != 1 {
		t.Fatal("expected 1 prepared")
	}
	if len(p.completed) != 1 {
		t.Fatal("expected 1 completed")
	}
}

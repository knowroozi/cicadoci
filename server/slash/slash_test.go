package slash

import "testing"

func TestParseStatus(t *testing.T) {
	cmd, err := Parse("status acme/cicadoci")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Verb != "status" {
		t.Fatalf("expected status, got %q", cmd.Verb)
	}
	repo, ref := cmd.StatusCmd()
	if repo != "acme/cicadoci" || ref != "" {
		t.Fatalf("unexpected: repo=%q ref=%q", repo, ref)
	}
}

func TestParseStatusWithRef(t *testing.T) {
	cmd, err := Parse("status acme/cicadoci main@main")
	if err != nil {
		t.Fatal(err)
	}
	repo, ref := cmd.StatusCmd()
	if repo != "acme/cicadoci" || ref != "main@main" {
		t.Fatalf("unexpected: repo=%q ref=%q", repo, ref)
	}
}

func TestParseRun(t *testing.T) {
	cmd, err := Parse("run acme/cicadoci main@main")
	if err != nil {
		t.Fatal(err)
	}
	repo, wf, ref := cmd.RunCmd()
	if repo != "acme/cicadoci" || wf != "main@main" || ref != "" {
		t.Fatalf("unexpected: repo=%q wf=%q ref=%q", repo, wf, ref)
	}
}

func TestParseRerun(t *testing.T) {
	cmd, err := Parse("rerun run-123 flaky")
	if err != nil {
		t.Fatal(err)
	}
	run, reason := cmd.RerunCmd()
	if run != "run-123" || reason != "flaky" {
		t.Fatalf("unexpected: run=%q reason=%q", run, reason)
	}
}

func TestParseLogs(t *testing.T) {
	cmd, err := Parse("logs run-123 test")
	if err != nil {
		t.Fatal(err)
	}
	run, step := cmd.LogsCmd()
	if run != "run-123" || step != "test" {
		t.Fatalf("unexpected: run=%q step=%q", run, step)
	}
}

func TestParseEmpty(t *testing.T) {
	_, err := Parse("")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestParseUnknownVerb(t *testing.T) {
	_, err := Parse("unknown acme/cicadoci")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestParseStatusTooFewArgs(t *testing.T) {
	_, err := Parse("status")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestParseRunTooFewArgs(t *testing.T) {
	_, err := Parse("run acme/cicadoci")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Also test too many
	_, err = Parse("run a b c d")
	if err == nil {
		t.Fatal("expected error for too many args, got nil")
	}
}

func TestParseRerunTooManyArgs(t *testing.T) {
	_, err := Parse("rerun r1 reason extra")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestParseLogsTooManyArgs(t *testing.T) {
	_, err := Parse("logs r1 s1 extra")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestVerbs(t *testing.T) {
	verbs := Verbs()
	if len(verbs) != 4 {
		t.Fatalf("expected 4 verbs, got %d", len(verbs))
	}
}

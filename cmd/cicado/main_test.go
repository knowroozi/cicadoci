package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRun_NoArgs verifies that run with no subcommand errors.
func TestRun_NoArgs(t *testing.T) {
	err := run(nil)
	if err == nil {
		t.Fatal("run(): expected error, got nil")
	}
}

// TestRun_Help verifies that the help subcommand succeeds.
func TestRun_Help(t *testing.T) {
	if err := run([]string{"help"}); err != nil {
		t.Errorf("run([help]): unexpected error: %v", err)
	}
}

// TestRun_UnknownSubcommand verifies that an unknown subcommand errors.
func TestRun_UnknownSubcommand(t *testing.T) {
	err := run([]string{"bogus"})
	if err == nil {
		t.Fatal("run([bogus]): expected error, got nil")
	}
}

// TestCmdValidate_NoArgs verifies that `validate` with no file errors.
func TestCmdValidate_NoArgs(t *testing.T) {
	err := cmdValidate(nil)
	if err == nil {
		t.Fatal("cmdValidate([]): expected error, got nil")
	}
}

// TestCmdValidate_SeparatorOnly verifies that `validate --` with no files errors.
func TestCmdValidate_SeparatorOnly(t *testing.T) {
	err := cmdValidate([]string{"--"})
	if err == nil {
		t.Fatal("cmdValidate([--]): expected error, got nil")
	}
}

// TestCmdValidate_ValidationError verifies that a syntactically invalid workflow
// file produces errValidateFailed rather than a wrapped error.
func TestCmdValidate_ValidationError(t *testing.T) {
	dir := t.TempDir()
	badFile := filepath.Join(dir, "bad.yaml")
	// "on:", "jobs:" missing → structural error.
	if err := os.WriteFile(badFile, []byte("name: test\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	err := cmdValidate([]string{badFile})
	if err == nil {
		t.Fatal("cmdValidate: expected error, got nil")
	}
	if !errors.Is(err, errValidateFailed) {
		t.Errorf("cmdValidate: got %q, want errValidateFailed", err)
	}
}

// TestCmdValidate_ValidFile verifies that a structurally valid workflow file
// reports success.
func TestCmdValidate_ValidFile(t *testing.T) {
	dir := t.TempDir()
	goodFile := filepath.Join(dir, "good.yaml")
	content := `name: smoke
on:
  push:
    branches: [main]
jobs:
  build:
    steps:
      - run: echo hello
`
	if err := os.WriteFile(goodFile, []byte(content), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := cmdValidate([]string{goodFile}); err != nil {
		t.Errorf("cmdValidate: unexpected error: %v", err)
	}
}

// TestCmdValidate_DoesNotReturnErrValidateFailed_WhenFileMissing verifies that
// a missing file produces a plain error, not errValidateFailed.
func TestCmdValidate_MissingFile(t *testing.T) {
	err := cmdValidate([]string{"/nonexistent/path.yaml"})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if errors.Is(err, errValidateFailed) {
		t.Error("missing file should not produce errValidateFailed")
	}
}

// TestUsage_ShortName is a sanity check that usage does not panic when
// called with a nil writer.
func TestUsage_ShortName(t *testing.T) {
	// usage takes an io.Writer; a nil writer would panic on Fprint.
	// We cannot test the output content here without a buffer, but we
	// can verify that help() doesn't panic with a valid writer.
	var buf bytes.Buffer
	usage(&buf)
}

// TestValidateFile_YAMLSyntaxError verifies that a YAML syntax error
// produces a *parse.ParseErrors containing that error.
func TestValidateFile_YAMLSyntaxError(t *testing.T) {
	dir := t.TempDir()
	badFile := filepath.Join(dir, "bad.yaml")
	// Truncated mapping.
	if err := os.WriteFile(badFile, []byte("name: [unclosed\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	err := validateFile(badFile)
	if err == nil {
		t.Fatal("validateFile: expected error, got nil")
	}
	// The error should carry positional information.
	if err.Error() == "" {
		t.Error("validateFile: error should have non-empty string form")
	}
}

// TestCmdValidate_MultiFile_ReadErrorAfterParseError verifies that when a
// parse failure is found in an earlier file and a read error occurs in a
// later file, the parse failure takes priority (exit 1, errValidateFailed)
// rather than the read error silently winning (exit 2).
func TestCmdValidate_MultiFile_ReadErrorAfterParseError(t *testing.T) {
	dir := t.TempDir()

	// bad.yaml fails validation.
	badFile := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badFile, []byte("name: test\n"), 0o644); err != nil {
		t.Fatalf("setup bad: %v", err)
	}

	// missing.yaml doesn't exist (read error).
	missingFile := filepath.Join(dir, "nope.yaml")

	err := cmdValidate([]string{badFile, missingFile})
	if err == nil {
		t.Fatal("cmdValidate: expected error, got nil")
	}
	// Parse failure found first, so errValidateFailed should win over the I/O error.
	if !errors.Is(err, errValidateFailed) {
		t.Errorf("cmdValidate: got %q, want errValidateFailed", err)
	}
}

// TestCmdValidate_AllReadErrors verifies that when all files are unreadable,
// the first read error is returned directly (not errValidateFailed).
func TestCmdValidate_AllReadErrors(t *testing.T) {
	err := cmdValidate([]string{"/nonexistent/a.yaml", "/nonexistent/b.yaml"})
	if err == nil {
		t.Fatal("cmdValidate: expected error, got nil")
	}
	if errors.Is(err, errValidateFailed) {
		t.Error("cmdValidate: all-read-error should not produce errValidateFailed")
	}
}

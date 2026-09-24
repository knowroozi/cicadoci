// Command cicado is the CICaDo CLI — a thin local tool for the same
// operations the HTTP API exposes, intended for developers working at a
// terminal and CI systems calling cicadoci in pre-push hooks.
//
// Subcommands:
//
//	cicado validate <file> [--] [<more files>]
//
// The `validate` subcommand parses and structurally validates a CICaDo
// workflow YAML file, printing one positional error (file:line:col: message)
// per problem found. Exit code is 0 when all files are valid, 1 when any
// file has errors, and 2 for usage errors.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"cicadoci.com/cicado/server/workflow/parse"
)

// exitValidateFailed is the exit code for a validation failure.
const exitValidateFailed = 1

func main() {
	var exitCode int
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "cicado: %v\n", err)
		if errors.Is(err, errValidateFailed) {
			exitCode = exitValidateFailed
		} else {
			exitCode = 2
		}
	}
	os.Exit(exitCode)
}

// errValidateFailed is the sentinel error returned by cmdValidate when a
// workflow file fails validation.
var errValidateFailed = errors.New("validation failed")

// run dispatches subcommands and returns an error for any usage or execution
// failure. os.Exit lives only in main, keeping run pure enough to test.
func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("no subcommand given")
	}

	switch args[0] {
	case "validate":
		return cmdValidate(args[1:])
	case "--help", "-h", "help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `cicado — CICaDo command-line tool

Usage:
  cicado <command> [args]

Commands:
  validate <file> [--] [<more files>]
      Parse and structurally validate CICaDo workflow YAML files.
  help
      Show this message.
`)
}

// cmdValidate is the entry point for `cicado validate`.
// It accepts one or more file paths; a leading "--" is a separator (ignored).
func cmdValidate(args []string) error {
	// Strip a leading -- separator if present.
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}

	if len(args) == 0 {
		return errors.New("validate: no file given; usage: cicado validate <file>")
	}

	anyParseFail := false
	for _, path := range args {
		if err := validateFile(path); err != nil {
			var pe *parse.ParseErrors
			if errors.As(err, &pe) {
				// Print each positional error on its own line.
				for _, err := range pe.Errors {
					fmt.Fprintln(os.Stderr, err)
				}
				anyParseFail = true
			} else {
				// Read-level failure (file not found, permissions, etc.).
				// Return the original error so callers can distinguish it
				// from a validation failure via errors.Is.
				fmt.Fprintf(os.Stderr, "cicado: %s: %v\n", path, err)
				return err
			}
		}
	}

	if anyParseFail {
		return errValidateFailed
	}
	return nil
}

// validateFile reads a workflow YAML file from disk, runs the full parse +
// structural-validation pipeline, and returns a *parse.ParseErrors carrying
// every positional error found. On success it prints "valid" to stderr so the
// CI output stays clean.
func validateFile(path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	fileName := filepath.Base(path)
	_, perrs, perr := parse.Parse(src, fileName)

	// parse.Parse returns (nil, nil, nil) on success and (nil, pe, pe) on
	// failure, so the third and second return values are the same pointer
	// when non-nil.  Use the third (error) as the guard.
	if perr != nil {
		return perr
	}

	// Defensive: a non-nil *ParseErrors with a nil error shouldn't happen,
	// but guard against it anyway.
	if perrs != nil && len(perrs.Errors) > 0 {
		return perrs
	}

	fmt.Fprintf(os.Stderr, "cicado: %s: valid\n", fileName)
	return nil
}

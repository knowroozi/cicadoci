// Package slash implements CICaDo Slack slash commands:
//
//	/ci status  <repo>
//	/ci run     <repo> <workflow> [ref]
//	/ci rerun   <run>
//	/ci logs    <run> [step]
//
// Each command maps a Slack identity to a CICaDo principal,
// invokes the public API, and routes the response back through
// the audit pipeline. All commands go through the same
// authentication and authorization layers as the web UI.
package slash

import (
	"fmt"
	"strings"
)

// Command is a parsed slash command.
type Command struct {
	// Verb is the subcommand: status, run, rerun, logs.
	Verb string

	// Args are the positional arguments.
	Args []string
}

// Parse parses a slash command payload.
//
// The payload is the raw text from Slack after the command prefix,
// e.g. "status acme/cicadoci", "run acme/cicadoci main@main".
func Parse(payload string) (Command, error) {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return Command{}, fmt.Errorf("slash: empty command")
	}
	// The first token is the verb.
	fields := strings.Fields(trimmed)
	cmd := Command{
		Verb: fields[0],
		Args: fields[1:],
	}
	switch cmd.Verb {
	case "status":
		if len(cmd.Args) < 1 {
			return Command{}, fmt.Errorf("slash: status requires <repo>")
		}
		if len(cmd.Args) > 2 {
			return Command{}, fmt.Errorf("slash: status accepts at most 2 args (repo [ref])")
		}
	case "run":
		if len(cmd.Args) < 2 {
			return Command{}, fmt.Errorf("slash: run requires <repo> <workflow>")
		}
		if len(cmd.Args) > 3 {
			return Command{}, fmt.Errorf("slash: run accepts at most 3 args (repo workflow [ref])")
		}
	case "rerun":
		if len(cmd.Args) < 1 {
			return Command{}, fmt.Errorf("slash: rerun requires <run>")
		}
		if len(cmd.Args) > 2 {
			return Command{}, fmt.Errorf("slash: rerun accepts at most 2 args (run [reason])")
		}
	case "logs":
		if len(cmd.Args) < 1 {
			return Command{}, fmt.Errorf("slash: logs requires <run>")
		}
		if len(cmd.Args) > 2 {
			return Command{}, fmt.Errorf("slash: logs accepts at most 2 args (run [step])")
		}
	default:
		return Command{}, fmt.Errorf("slash: unknown verb %q (want status|run|rerun|logs)", cmd.Verb)
	}
	return cmd, nil
}

// StatusCmd returns the parsed status command's args.
func (c Command) StatusCmd() (repo, ref string) {
	repo = c.Args[0]
	if len(c.Args) > 1 {
		ref = c.Args[1]
	}
	return
}

// RunCmd returns the parsed run command's args.
func (c Command) RunCmd() (repo, workflow, ref string) {
	repo = c.Args[0]
	workflow = c.Args[1]
	if len(c.Args) > 2 {
		ref = c.Args[2]
	}
	return
}

// RerunCmd returns the parsed rerun command's args.
func (c Command) RerunCmd() (run, reason string) {
	run = c.Args[0]
	if len(c.Args) > 1 {
		reason = c.Args[1]
	}
	return
}

// LogsCmd returns the parsed logs command's args.
func (c Command) LogsCmd() (run, step string) {
	run = c.Args[0]
	if len(c.Args) > 1 {
		step = c.Args[1]
	}
	return
}

// Verbs lists all valid verb names.
func Verbs() []string {
	return []string{"status", "run", "rerun", "logs"}
}

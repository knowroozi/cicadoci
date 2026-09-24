// Package parse provides a positional YAML parser for CICaDo workflow files.
//
// It produces the same Go types defined in the sibling `workflow` package and
// returns precise, human- and agent-readable error messages carrying (file,
// line, column, message, fix-hint) on every failure.
//
// The parser deliberately targets the constrained §6 guardrail: no loops, no
// expression language, no `if:` beyond the {success, failure, always} enum,
// no templating.
package parse

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"cicadoci.com/cicado/server/workflow"
)

// nodePos holds a 1-based position in a YAML source file.
type nodePos struct {
	Line   int
	Column int
}

// ParseError carries one positional error from the parser.
type ParseError struct {
	File    string
	Line    int
	Column  int
	Message string
	Hint    string
}

func (e *ParseError) Error() string {
	pos := fmt.Sprintf("%s:%d:%d", e.File, e.Line, e.Column)
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s (hint: %s)", pos, e.Message, e.Hint)
	}
	return fmt.Sprintf("%s: %s", pos, e.Message)
}

// ParseErrors is a collection of positional errors.
type ParseErrors struct {
	Errors []*ParseError
	// positions records top-level key positions so structural
	// validators reference the real node location, not {1,1}.
	positions map[string]nodePos
}

func (pe *ParseErrors) pos(key string, n *yaml.Node) {
	if pe.positions == nil {
		pe.positions = make(map[string]nodePos)
	}
	pe.positions[key] = posOf(n)
}

func (pe *ParseErrors) at(key string) nodePos {
	if p, ok := pe.positions[key]; ok {
		return p
	}
	return nodePos{Line: 1, Column: 1}
}

func (pe *ParseErrors) Error() string {
	var b strings.Builder
	for i, e := range pe.Errors {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(e.Error())
	}
	return b.String()
}

func add(pe *ParseErrors, file string, pos nodePos, msg, hint string) {
	pe.Errors = append(pe.Errors, &ParseError{
		File:    file,
		Line:    pos.Line,
		Column:  pos.Column,
		Message: msg,
		Hint:    hint,
	})
}

// posOf extracts position info from a yaml.Node, or returns the zero position.
func posOf(n *yaml.Node) nodePos {
	if n == nil {
		return nodePos{Line: 0, Column: 0}
	}
	return nodePos{Line: n.Line, Column: n.Column}
}

// Parse is the entry point for parsing a YAML workflow file.
//
// It returns a fully-parsed *workflow.Workflow on success. On failure it
// returns nil and a *ParseErrors containing all positional errors discovered
// (the parser does not stop at the first error — it attempts to report as
// many as it can).
//
// The fileName argument is used in error messages; it need not be a real
// path (callers can pass e.g. "cicadoci.yaml" or "<stdin>").
func Parse(src []byte, fileName string) (*workflow.Workflow, *ParseErrors, error) {
	pe := &ParseErrors{}

	// First pass: unmarshal into a yaml.Node tree to get position info.
	var rootNode yaml.Node
	if err := yaml.Unmarshal(src, &rootNode); err != nil {
		add(pe, fileName, nodePos{Line: 1, Column: 1},
			"YAML syntax error: "+err.Error(),
			"check indentation and quoting in the file")
		return nil, pe, pe
	}

	// The root is a DocumentNode; its first child is the actual root value.
	if rootNode.Kind != yaml.DocumentNode || len(rootNode.Content) == 0 {
		add(pe, fileName, posOf(&rootNode),
			"empty or non-mapping document",
			"a workflow file must contain a YAML mapping (key: value pairs)")
		return nil, pe, pe
	}

	body := rootNode.Content[0]
	if body == nil || body.Kind != yaml.MappingNode {
		add(pe, fileName, posOf(&rootNode),
			"document root is not a mapping",
			"the top level of a workflow file must be a YAML mapping (key: value pairs)")
		return nil, pe, pe
	}

	wf, perr := parseDoc(body, fileName)
	if perr != nil {
		pe.Errors = append(pe.Errors, perr.Errors...)
	}

	// Structural validation: empty name, no triggers, no jobs.
	if wf != nil {
		validateStructural(wf, fileName, pe)
	}

	if len(pe.Errors) > 0 {
		return nil, pe, pe
	}
	return wf, nil, nil
}

// stepKeyTimeout is the YAML key for the per-job and per-step timeout field.
const stepKeyTimeout = "timeout"

// parseDoc walks a top-level YAML mapping node and produces a *Workflow.
func parseDoc(node *yaml.Node, fileName string) (*workflow.Workflow, *ParseErrors) {
	pe := &ParseErrors{}
	wf := &workflow.Workflow{}

	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		key := keyNode.Value

		switch key {
		case "name":
			pe.pos("name", keyNode)
			if valNode.Kind != yaml.ScalarNode {
				add(pe, fileName, posOf(keyNode),
					"'name' must be a string", "example: name: deploy-api")
				continue
			}
			wf.Name = valNode.Value

		case "on":
			pe.pos("on", keyNode)
			if valNode.Kind != yaml.MappingNode {
				add(pe, fileName, posOf(keyNode),
					"'on' must be a mapping of trigger types to their configuration",
					"example: on:\n  push: { branches: [main] }")
				continue
			}
			wf.On = parseOn(valNode, fileName, pe)

		case "jobs":
			pe.pos("jobs", keyNode)
			if valNode.Kind != yaml.MappingNode {
				add(pe, fileName, posOf(keyNode),
					"'jobs' must be a mapping of job-name to job specification",
					"example: jobs:\n  build:\n    runs-on: [linux]\n    steps: [...]")
				continue
			}
			wf.Jobs = parseJobs(valNode, fileName, pe)

		default:
			add(pe, fileName, posOf(keyNode),
				fmt.Sprintf("unknown top-level key %q", key),
				"valid top-level keys are: name, on, jobs")
		}
	}

	if len(pe.Errors) > 0 {
		return nil, pe
	}
	return wf, nil
}

// validateStructural adds structural-validation errors to pe.
func validateStructural(wf *workflow.Workflow, fileName string, pe *ParseErrors) {
	if wf.Name == "" {
		add(pe, fileName, pe.at("name"),
			"workflow name is empty",
			"add 'name: <your-workflow-name>' at the top of the file")
	}
	if !hasTrigger(&wf.On) {
		add(pe, fileName, pe.at("on"),
			"workflow has no triggers",
			"add at least one trigger under 'on:', e.g. on:\n  push:\n    branches: [main]")
	}
	if len(wf.Jobs) == 0 {
		add(pe, fileName, pe.at("jobs"),
			"workflow has no jobs",
			"add at least one job under 'jobs:', e.g. jobs:\n  build:\n    steps: [...]")
	}
}

func hasTrigger(o *workflow.On) bool {
	if o == nil {
		return false
	}
	return o.Push != nil || o.PullRequest != nil ||
		o.Schedule != nil || len(o.Additional) > 0
}

func parseOn(node *yaml.Node, fileName string, pe *ParseErrors) workflow.On {
	var on workflow.On
	if node.Kind != yaml.MappingNode {
		return on
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		key := keyNode.Value
		switch key {
		case "push":
			on.Push = parsePushTrigger(valNode, fileName, pe)
		case "pull_request":
			on.PullRequest = parsePullRequestTrigger(valNode, fileName, pe)
		case "schedule":
			on.Schedule = parseScheduleTrigger(valNode, fileName, pe)
		default:
			add(pe, fileName, posOf(keyNode),
				fmt.Sprintf("unknown trigger type %q", key),
				"valid trigger types are: push, pull_request, schedule")
		}
	}
	return on
}

func parsePushTrigger(node *yaml.Node, fileName string, pe *ParseErrors) *workflow.PushTrigger {
	t := &workflow.PushTrigger{}
	if node.Kind != yaml.MappingNode {
		add(pe, fileName, posOf(node), "push config must be a mapping",
			"example: push:\n  branches: [main]")
		return t
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		switch keyNode.Value {
		case "branches":
			t.Branches = parseStringList(valNode, fileName, pe, "push.branches")
		case "tags":
			t.Tags = parseStringList(valNode, fileName, pe, "push.tags")
		case "paths":
			t.Paths = parseStringList(valNode, fileName, pe, "push.paths")
		default:
			add(pe, fileName, posOf(keyNode),
				fmt.Sprintf("unknown key %q in push config", keyNode.Value),
				"valid keys: branches, tags, paths")
		}
	}
	return t
}

func parsePullRequestTrigger(node *yaml.Node, fileName string, pe *ParseErrors) *workflow.PullRequestTrigger {
	t := &workflow.PullRequestTrigger{}
	if node.Kind != yaml.MappingNode {
		add(pe, fileName, posOf(node), "pull_request config must be a mapping",
			"example: pull_request:\n  branches: [main]")
		return t
	}
	var rawTypes []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		switch keyNode.Value {
		case "branches":
			t.Branches = parseStringList(valNode, fileName, pe, "pull_request.branches")
		case "tags":
			t.Tags = parseStringList(valNode, fileName, pe, "pull_request.tags")
		case "types":
			rawTypes = parseStringList(valNode, fileName, pe, "pull_request.types")
		default:
			add(pe, fileName, posOf(keyNode),
				fmt.Sprintf("unknown key %q in pull_request config", keyNode.Value),
				"valid keys: branches, tags, types")
		}
	}
	for _, tp := range rawTypes {
		switch tp {
		case "opened", "synchronize", "reopened":
		default:
			add(pe, fileName, posOf(node),
				fmt.Sprintf("unknown PR event type %q", tp),
				"valid types: opened, synchronize, reopened")
		}
	}
	if len(rawTypes) > 0 {
		t.Types = rawTypes
	}
	return t
}

func parseScheduleTrigger(node *yaml.Node, fileName string, pe *ParseErrors) *workflow.ScheduleTrigger {
	t := &workflow.ScheduleTrigger{}
	if node.Kind != yaml.MappingNode {
		add(pe, fileName, posOf(node), "schedule config must be a mapping",
			"example: schedule:\n  cron: ['0 0 * * *']")
		return t
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		switch keyNode.Value {
		case "cron":
			t.Cron = parseStringList(valNode, fileName, pe, "schedule.cron")
		default:
			add(pe, fileName, posOf(keyNode),
				fmt.Sprintf("unknown key %q in schedule config", keyNode.Value),
				"valid keys: cron")
		}
	}
	if len(t.Cron) == 0 {
		add(pe, fileName, posOf(node),
			"schedule.cron must have at least one expression",
			"example: cron: ['0 0 * * *']")
	}
	return t
}

func parseJobs(node *yaml.Node, fileName string, pe *ParseErrors) workflow.Jobs {
	jobs := workflow.Jobs{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		jobNode := node.Content[i+1]
		name := keyNode.Value
		if jobNode.Kind != yaml.MappingNode {
			add(pe, fileName, posOf(keyNode),
				fmt.Sprintf("job %q must be a mapping", name),
				"each job must define at least a 'steps' key")
			continue
		}

		// Structural: at least one step?
		hasSteps := false
		for j := 0; j+1 < len(jobNode.Content); j += 2 {
			if jobNode.Content[j].Value == "steps" {
				hasSteps = true
				break
			}
		}
		if !hasSteps {
			add(pe, fileName, posOf(keyNode),
				fmt.Sprintf("job %q has no 'steps' key", name),
				"every job must have at least one step")
		}

		job := parseJob(jobNode, name, fileName, pe)
		jobs[name] = job
	}
	return jobs
}

func parseJob(node *yaml.Node, name string, fileName string, pe *ParseErrors) *workflow.Job {
	job := &workflow.Job{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		key := keyNode.Value
		switch key {
		case "runs-on":
			job.RunsOn = parseStringList(valNode, fileName, pe, "runs-on")
		case "needs":
			if valNode.Kind == yaml.SequenceNode {
				for _, item := range valNode.Content {
					if item.Kind != yaml.ScalarNode {
						add(pe, fileName, posOf(node),
							"'needs' list items must be strings (job names)",
							"example: needs: [test, build]")
						continue
					}
					job.Needs = append(job.Needs, item.Value)
				}
			} else if valNode.Kind == yaml.ScalarNode {
				job.Needs = workflow.Needs{valNode.Value}
			} else {
				add(pe, fileName, posOf(node),
					"'needs' must be a string or a list of strings",
					"example: needs: test  or  needs: [test, build]")
			}
		case "steps":
			if valNode.Kind != yaml.SequenceNode {
				add(pe, fileName, posOf(keyNode),
					"'steps' must be a list of step definitions",
					"example: steps:\n   - name: hello\n     run: echo hello")
				break
			}
			job.Steps = parseSteps(valNode, name, fileName, pe)
			if len(job.Steps) == 0 {
				add(pe, fileName, posOf(keyNode),
					fmt.Sprintf("job %q has empty steps", name),
					"every job must define at least one step")
			}
		case "approval":
			if valNode.Kind != yaml.MappingNode {
				add(pe, fileName, posOf(keyNode),
					"'approval' must be a mapping",
					"example: approval: { channel: '#deploys', allow: [user:@alice] }")
				break
			}
			job.Approval = parseApprovalGate(valNode, name, fileName, pe)
		case "image":
			if valNode.Kind != yaml.ScalarNode {
				add(pe, fileName, posOf(keyNode),
					"'image' must be a string", "example: image: golang:1.24")
			} else {
				job.Image = valNode.Value
			}
		case "env":
			if valNode.Kind != yaml.MappingNode {
				add(pe, fileName, posOf(keyNode),
					"'env' must be a mapping of string to string",
					"example: env:\n  FOO: bar")
			} else {
				job.Env = parseEnvMap(valNode, fileName, pe)
			}
		case stepKeyTimeout:
			job.TimeoutSeconds = parseInt(valNode, fileName, pe, "job '"+name+"' timeout", "timeout: 300")
		default:
			add(pe, fileName, posOf(keyNode),
				fmt.Sprintf("unknown key %q in job %q", key, name),
				"valid keys: runs-on, needs, steps, approval, image, env, timeout")
		}
	}
	return job
}

func parseSteps(node *yaml.Node, jobName string, fileName string, pe *ParseErrors) []*workflow.Step {
	var steps []*workflow.Step
	idx := 0
	for _, stepNode := range node.Content {
		idx++
		if stepNode.Kind != yaml.MappingNode {
			add(pe, fileName, posOf(stepNode),
				fmt.Sprintf("job %q step %d: must be a mapping", jobName, idx),
				"each step must be a mapping with 'run' or 'uses'")
			continue
		}
		step := &workflow.Step{}
		hasUses, hasRun := false, false
		for i := 0; i+1 < len(stepNode.Content); i += 2 {
			keyNode := stepNode.Content[i]
			field := stepNode.Content[i+1]
			key := keyNode.Value
			switch key {
			case "name":
				step.Name = scalarOrWarn(field, "name", jobName, idx, fileName, pe)
			case "uses":
				step.Uses = scalarOrWarn(field, "uses", jobName, idx, fileName, pe)
				hasUses = true
			case "run":
				step.Run = scalarOrWarn(field, "run", jobName, idx, fileName, pe)
				hasRun = true
			case "image":
				step.Image = scalarOrWarn(field, "image", jobName, idx, fileName, pe)
			case "identity":
				id := scalarOrWarn(field, "identity", jobName, idx, fileName, pe)
				step.Identity = id
				if id != "" && !isValidIdentity(id) {
					add(pe, fileName, posOf(keyNode),
						fmt.Sprintf("job %q step %d: invalid 'identity' %q", jobName, idx, id),
						"valid identity values: none, aws")
				}
			case "if":
				cond := scalarOrWarn(field, "if", jobName, idx, fileName, pe)
				step.If = cond
				if cond != "" && !isValidIfCond(cond) {
					add(pe, fileName, posOf(keyNode),
						fmt.Sprintf("job %q step %d: invalid 'if' value %q", jobName, idx, cond),
						"valid values: success, failure, always")
				}
			case stepKeyTimeout:
				step.TimeoutSeconds = parseInt(field, fileName, pe, fmt.Sprintf("job %q step %d timeout", jobName, idx), "timeout: 300")
			case "network":
				if field.Kind == yaml.ScalarNode {
					if field.Tag == "!!bool" || field.Tag == "?" {
						step.NetworkEnabled = field.Value == "true"
					} else {
						add(pe, fileName, posOf(keyNode),
							fmt.Sprintf("job %q step %d: 'network' must be a boolean", jobName, idx),
							"example: network: true")
					}
				} else {
					add(pe, fileName, posOf(keyNode),
						fmt.Sprintf("job %q step %d: 'network' must be a boolean", jobName, idx),
						"example: network: true")
				}
			case "env":
				if field.Kind != yaml.MappingNode {
					add(pe, fileName, posOf(keyNode),
						fmt.Sprintf("job %q step %d: 'env' must be a mapping", jobName, idx),
						"example: env:\n  FOO: bar")
					break
				}
				step.Env = parseEnvMap(field, fileName, pe)
			default:
				add(pe, fileName, posOf(keyNode),
					fmt.Sprintf("job %q step %d: unknown key %q", jobName, idx, key),
					"valid keys: name, uses, run, image, identity, if, timeout, network, env")
			}
		}

		if hasUses && hasRun {
			add(pe, fileName, posOf(stepNode),
				fmt.Sprintf("job %q step %d: 'uses' and 'run' are mutually exclusive", jobName, idx),
				"use exactly one of 'uses' or 'run'")
		}
		if !hasUses && !hasRun {
			add(pe, fileName, posOf(stepNode),
				fmt.Sprintf("job %q step %d: must have either 'uses' or 'run'", jobName, idx),
				"example: run: echo hello")
		}

		steps = append(steps, step)
	}
	return steps
}

func parseApprovalGate(node *yaml.Node, jobName string, fileName string, pe *ParseErrors) *workflow.ApprovalGate {
	gate := &workflow.ApprovalGate{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		key := keyNode.Value
		switch key {
		case "channel":
			gate.Channel = scalarOrWarn(valNode, "approval.channel", jobName, 0, fileName, pe)
		case "allow":
			gate.Allow = parseStringList(valNode, fileName, pe, fmt.Sprintf("job %q approval.allow", jobName))
		case stepKeyTimeout:
			gate.TimeoutSeconds = parseInt(valNode, fileName, pe, fmt.Sprintf("job %q approval.timeout", jobName), "timeout: 300")
		default:
			add(pe, fileName, posOf(keyNode),
				fmt.Sprintf("job %q approval: unknown key %q", jobName, key),
				"valid keys: channel, allow, timeout")
		}
	}
	if gate.Channel == "" {
		add(pe, fileName, posOf(node),
			fmt.Sprintf("job %q approval: missing required 'channel'", jobName),
			"example: approval: { channel: '#deploys', allow: [user:@alice] }")
	}
	if len(gate.Allow) == 0 {
		add(pe, fileName, posOf(node),
			fmt.Sprintf("job %q approval: missing required 'allow' list", jobName),
			"example: allow: [user:@alice]")
	}
	return gate
}

// --- helpers ---

func parseStringList(node *yaml.Node, fileName string, pe *ParseErrors, path string) []string {
	if node.Kind == yaml.ScalarNode {
		return []string{node.Value}
	}
	if node.Kind != yaml.SequenceNode {
		add(pe, fileName, posOf(node),
			fmt.Sprintf("%s must be a string or a list of strings", path),
			"example: [main, develop]")
		return nil
	}
	var out []string
	for _, item := range node.Content {
		if item.Kind != yaml.ScalarNode {
			add(pe, fileName, posOf(item),
				fmt.Sprintf("%s items must be strings", path),
				"example: [main, develop]")
			continue
		}
		out = append(out, item.Value)
	}
	return out
}

func parseEnvMap(node *yaml.Node, fileName string, pe *ParseErrors) map[string]string {
	env := map[string]string{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		if keyNode.Kind != yaml.ScalarNode {
			add(pe, fileName, posOf(keyNode),
				"env keys must be strings", "example: FOO: bar")
			continue
		}
		if valNode.Kind != yaml.ScalarNode {
			add(pe, fileName, posOf(node),
				fmt.Sprintf("env value for %q must be a string", keyNode.Value),
				"example: FOO: bar")
			continue
		}
		env[keyNode.Value] = valNode.Value
	}
	return env
}

func scalarOrWarn(node *yaml.Node, key, jobName string, stepIdx int, fileName string, pe *ParseErrors) string {
	if node.Kind != yaml.ScalarNode {
		loc := fmt.Sprintf("job %q %s", jobName, key)
		if stepIdx > 0 {
			loc = fmt.Sprintf("job %q step %d %s", jobName, stepIdx, key)
		}
		add(pe, fileName, posOf(node),
			fmt.Sprintf("%s: must be a string", loc), "example: "+key+": my-value")
		return ""
	}
	return node.Value
}

func parseInt(node *yaml.Node, fileName string, pe *ParseErrors, path, hintText string) int {
	if node.Kind != yaml.ScalarNode {
		add(pe, fileName, posOf(node),
			fmt.Sprintf("%s must be an integer (seconds)", path), "example: "+hintText)
		return 0
	}
	val, err := strconv.Atoi(node.Value)
	if err != nil {
		add(pe, fileName, posOf(node),
			fmt.Sprintf("%s: not a valid integer: %q", path, node.Value),
			"example: "+hintText)
		return 0
	}
	if val < 0 {
		add(pe, fileName, posOf(node),
			fmt.Sprintf("%s: negative values are not allowed", path),
			"example: "+hintText)
		return 0
	}
	return val
}

func isValidIfCond(s string) bool {
	switch s {
	case "success", "failure", "always", "":
		return true
	default:
		return false
	}
}

func isValidIdentity(s string) bool {
	switch s {
	case "", "none", "aws":
		return true
	default:
		return false
	}
}

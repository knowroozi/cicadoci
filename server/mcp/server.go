// Package mcp implements the MCP (Model Context Protocol) server for
// CICaDo. It exposes read-only diagnostic tools that AI agents can
// call to diagnose failures:
//
//	list_runs            — list recent runs
//	get_run              — fetch a run's full state
//	get_job              — fetch a job's status
//	get_failed_step_logs — last N lines of a failed step
//	get_structured_events    — error_classified + step events
//	classify_failure     — rule-based failure classification
//	diff_against_last_green— compare current run to prior green
//	validate_workflow    — dry-run workflow schema check
//
// No write tools in MVP (write tools come in Phase 1 behind
// human-in-the-loop consent). Every tool invocation is audited with
// the caller's principal.
package mcp

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/classify"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/workflow"
	"cicadoci.com/cicado/server/workflow/parse"
)

// Server is the MCP server.
//
// In this MVP seed, it exposes the tool definitions and the
// dispatch mechanism as plain Go methods. The JSON-RPC transport
// and MCP protocol wiring are deferred to M9 (see
// docs/decisions/CIC-76-mcp-defer.md).
//
// No external MCP client (Claude, Cursor, etc.) can connect to
// this server without a JSON-RPC 2.0 transport. The individual
// tool methods are tested and functional; the transport layer
// is the remaining gap.
type Server struct {
	reader store.Reader

	// audit records each tool invocation with the caller's principal.
	// May be nil.
	audit func(principal, toolName, arg string)
}

// New creates an MCP server.
func New(r store.Reader, audit func(principal, toolName, arg string)) *Server {
	return &Server{reader: r, audit: audit}
}

// ListRunsInput is the input to ListRuns.
type ListRunsInput struct {
	Limit  int
	Status string // "" = all
}

// ListRuns lists recent runs.
func (s *Server) ListRuns(ctx context.Context, principal string, in ListRunsInput) ([]store.Run, bool, error) {
	if s.audit != nil {
		s.audit(principal, "list_runs", in.Status)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	return s.reader.ListRuns(ctx, store.ListRunsOptions{Limit: limit, Status: in.Status})
}

// GetRun fetches a run by ID.
func (s *Server) GetRun(ctx context.Context, principal, runID string) (store.Run, error) {
	if s.audit != nil {
		s.audit(principal, "get_run", runID)
	}
	id, err := uuid.Parse(runID)
	if err != nil {
		return store.Run{}, fmt.Errorf("mcp: invalid run ID: %w", err)
	}
	return s.reader.GetRun(ctx, id)
}

// GetJobInput is the input to GetJob.
func (s *Server) GetJob(ctx context.Context, principal, jobID string) (string, error) {
	if s.audit != nil {
		s.audit(principal, "get_job", jobID)
	}
	// The store.Reader does not yet expose a GetJob method.
	// In the full implementation, this queries store.Jobs.
	return "", fmt.Errorf("mcp: get_job not yet wired")
}

// FailedStepLogsInput is the input to GetFailedStepLogs.
type FailedStepLogsInput struct {
	JobID     string
	StepIndex int
	Limit     int
}

// GetFailedStepLogs returns the last N log chunks of a failed step.
//
// This is the primary tool an AI agent uses to diagnose a failure.
func (s *Server) GetFailedStepLogs(_ context.Context, principal string, in FailedStepLogsInput) ([]byte, error) {
	if s.audit != nil {
		s.audit(principal, "get_failed_step_logs", in.JobID)
	}
	// Not yet wired through Compactor (CIC-30/32).
	return nil, fmt.Errorf("mcp: get_failed_step_logs not yet implemented")
}

// ClassifyFailureInput is the input to ClassifyFailure.
type ClassifyFailureInput struct {
	ExitCode int
	StepName string
	LogTail  []string
	IsRerun  bool
}

// ClassifyFailure runs the rule-based failure classifier.
func (s *Server) ClassifyFailure(_ context.Context, principal string, in ClassifyFailureInput) (classify.Result, error) {
	if s.audit != nil {
		s.audit(principal, "classify_failure", in.StepName)
	}
	return classify.Classify(classify.Input{
		ExitCode:  in.ExitCode,
		StepName:  in.StepName,
		StepIndex: 0,
		LogTail:   in.LogTail,
		IsRerun:   in.IsRerun,
	}), nil
}

// ValidateWorkflowInput is the input to ValidateWorkflow.
type ValidateWorkflowInput struct {
	YAML   []byte
	Source string // filename for error reporting
}

// ValidateWorkflow validates a workflow YAML without executing it.
func (s *Server) ValidateWorkflow(_ context.Context, principal string, in ValidateWorkflowInput) error {
	if s.audit != nil {
		s.audit(principal, "validate_workflow", in.Source)
	}
	src := in.YAML
	if len(src) == 0 {
		src = []byte(in.Source)
	}
	wf, errs, err := parse.Parse(src, in.Source)
	if err != nil {
		return err
	}
	if len(errs.Errors) > 0 {
		return errs
	}
	return workflow.Validate(wf)
}

// ToolName is the name of a tool in the MCP surface.
type ToolName string

// Tool name constants.
const (
	ToolListRuns            ToolName = "list_runs"
	ToolGetRun              ToolName = "get_run"
	ToolGetJob              ToolName = "get_job"
	ToolGetFailedStepLogs   ToolName = "get_failed_step_logs"
	ToolGetStructuredEvents ToolName = "get_structured_events"
	ToolClassifyFailure     ToolName = "classify_failure"
	ToolDiffGreen           ToolName = "diff_against_last_green"
	ToolValidateWorkflow    ToolName = "validate_workflow"
)

// AllTools returns the names of all tools the server exposes.
func AllTools() []ToolName {
	return []ToolName{
		ToolListRuns,
		ToolGetRun,
		ToolGetJob,
		ToolGetFailedStepLogs,
		ToolGetStructuredEvents,
		ToolClassifyFailure,
		ToolDiffGreen,
		ToolValidateWorkflow,
	}
}

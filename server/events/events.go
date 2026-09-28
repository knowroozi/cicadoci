// Package events defines the structured event taxonomy for CICaDo runs.
//
// StructuredEvents are the machine-readable counterpart to raw log chunks:
//
//   - kind = "chunk" → raw log bytes (stdout/stderr interleaved)
//   - kind = "event" → a structured event with a well-known event_kind
//
// Each event_kind has a JSON schema. The schemas live here as compile-time
// constants. `IsValidKind` is available for on-ingest validation to prevent
// malformed events from corrupting the log_entries table (the DB constraint
// `log_entries_shape` enforces the row-level invariant, but the taxonomy
// check is not yet wired at the ingest path — that is a follow-up).
//
// The taxonomy is published alongside the workflow JSON schema so that
// UI consumers, MCP query tools, and the failure classifier all share
// one source of truth for "what event kinds exist and what fields they
// carry".
package events

// Event kinds are the well-known event_kind string values stored in the
// log_entries table.
const (
	// StepStarted is emitted when a job step begins execution.
	//
	// Schema: {"step_index": int, "step_name": string, "ts": RFC3339}
	StepStarted = "step_started"

	// StepFinished is emitted when a job step completes.
	//
	// Schema: {"step_index": int, "step_name": string, "exit_code": int, "duration_ms": int, "ts": RFC3339}
	StepFinished = "step_finished"

	// ErrorClassified is emitted when a step failure is classified
	// into a known failure mode. This is the substrate for
	// classify_failure in the MCP tool surface.
	//
	// Schema: {"step_index": int, "classification": string, "confidence": float, "ts": RFC3339}
	ErrorClassified = "error_classified"

	// ArtifactUploaded is emitted when a runner uploads an artifact
	// to object storage.
	//
	// Schema: {"artifact_name": string, "blob_key": string, "size_bytes": int, "content_type": string, "ts": RFC3339}
	ArtifactUploaded = "artifact_uploaded"

	// LogGap is emitted when the server detects a gap in the log
	// sequence — a missing seq between two consecutive entries.
	//
	// Schema: {"from_seq": int, "to_seq": int, "ts": RFC3339}
	LogGap = "log_gap"
)

// ValidKinds is the set of all known event kinds.
var ValidKinds = map[string]bool{
	StepStarted:      true,
	StepFinished:     true,
	ErrorClassified:  true,
	ArtifactUploaded: true,
	LogGap:           true,
}

// IsValidKind reports whether k is a known event kind.
func IsValidKind(k string) bool { return ValidKinds[k] }

// EventStepStarted carries the fields for a step_started event.
type EventStepStarted struct {
	StepIndex int    `json:"step_index"`
	StepName  string `json:"step_name"`
	Ts        string `json:"ts"`
}

// EventStepFinished carries the fields for a step_finished event.
type EventStepFinished struct {
	StepIndex  int    `json:"step_index"`
	StepName   string `json:"step_name"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	Ts         string `json:"ts"`
}

// EventErrorClassified carries the fields for an error_classified event.
type EventErrorClassified struct {
	StepIndex      int     `json:"step_index"`
	Classification string  `json:"classification"`
	Confidence     float64 `json:"confidence"`
	Ts             string  `json:"ts"`
}

// ArtifactEvent carries the fields for an artifact_uploaded event.
type ArtifactEvent struct {
	ArtifactName string `json:"artifact_name"`
	BlobKey      string `json:"blob_key"`
	SizeBytes    int64  `json:"size_bytes"`
	ContentType  string `json:"content_type"`
	Ts           string `json:"ts"`
}

// LogGapEvent carries the fields for a log_gap event.
type LogGapEvent struct {
	FromSeq int64  `json:"from_seq"`
	ToSeq   int64  `json:"to_seq"`
	Ts      string `json:"ts"`
}

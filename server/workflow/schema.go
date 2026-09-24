package workflow

import _ "embed" // embed the JSON schema at compile time.

//go:embed workflow-v1.json
var SchemaV1 []byte

// SchemaV1String returns the raw bytes of the published JSON Schema v1.
//
// This is the string a consumer passes to a JSON-Schema validator; it is the
// canonical document that the MCP `validate_workflow` tool will serve.
func SchemaV1String() string {
	return string(SchemaV1)
}

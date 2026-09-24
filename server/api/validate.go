// validate.go implements POST /api/v1/validate — the stateless workflow
// validation endpoint. It is exposed in M1 as a plain HTTP route and later
// wrapped as the MCP `validate_workflow` tool with no change to the handler.
//
// The endpoint accepts a JSON body containing a YAML source string and an
// optional file name, runs the full parse + structural-validation pipeline,
// and returns a uniform JSON response with every positional error found.
//
// It requires no database interaction: workflow validation is purely syntactic
// and structural, which is why it lives on the same handler as the read
// surface but never touches store.Reader.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"cicadoci.com/cicado/server/workflow/parse"
)

// maxValidateBody caps the request body size for /validate so a hostile or
// misconfigured client can't hold a megabyte of YAML in memory for 30 s.
const maxValidateBody = 1 << 20 // 1 MiB

// ── request / response types ────────────────────────────────────

// validateRequest is the JSON body for POST /api/v1/validate.
type validateRequest struct {
	// YAML is the raw workflow source to validate.
	YAML string `json:"yaml"`
	// FileName is the logical file name reported in positional errors.
	// Optional; defaults to "workflow.yaml" when omitted.
	FileName string `json:"fileName,omitempty"`
}

// validateResponse is the JSON body returned by POST /api/v1/validate.
type validateResponse struct {
	// Valid is true when no positional errors were found.
	Valid bool `json:"valid"`
	// Errors holds one entry per positional problem found.
	// Empty when Valid is true.
	Errors []parseErrorDetail `json:"errors,omitempty"`
}

// parseErrorDetail is one positional error, JSON-serialisable. It mirrors the
// fields of parse.ParseError but uses a distinct name to avoid collision when
// both types coexist in the same package.
type parseErrorDetail struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
	// Hint is an optional fix suggestion; empty when the parser has none.
	Hint string `json:"hint,omitempty"`
}

// ── handler ─────────────────────────────────────────────────────

// handleValidate parses a JSON request body, runs parse + structural
// validation on the supplied YAML, and writes a uniform JSON response.
//
// HTTP status semantics:
//   - 200 OK        — the request was well-formed and validation ran.  The
//     response body carries valid=true/false and the positional error list.
//   - 400 BadRequest — the request body itself was malformed or exceeded the
//     body-size cap.
func (h *Handler) handleValidate(w http.ResponseWriter, r *http.Request) {
	// Bound the body size up front; a 1 MiB cap is generous for a workflow
	// file and prevents a hostile client from stalling the handler.
	r.Body = http.MaxBytesReader(w, r.Body, maxValidateBody)

	var req validateRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		// io.EOF means an empty body (a 400); a malformed JSON body is too.
		writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest,
			"malformed request body: expected JSON with a 'yaml' field", err)
		return
	}

	fileName := req.FileName
	if fileName == "" {
		fileName = "workflow.yaml"
	}

	// Run the full parse + structural-validation pipeline.
	// parse.Parse returns (nil, pe, pe) on any error and (wf, nil, nil) on
	// success, so the third return value is the error guard and perr is
	// always a *parse.ParseErrors when non-nil.
	_, _, perr := parse.Parse([]byte(req.YAML), fileName)
	if perr == nil {
		writeJSON(w, http.StatusOK, validateResponse{Valid: true})
		return
	}

	var perrs *parse.ParseErrors
	// parse.Parse always wraps a *ParseErrors on failure, so this assertion
	// is always true. We still use errors.As for type safety rather than a
	// raw assert, to avoid panicking if the contract ever changes.
	if !errors.As(perr, &perrs) {
		writeError(w, h.logger, http.StatusInternalServerError, errCodeInternal,
			"unexpected parse error type", perr)
		return
	}

	resp := validateResponse{Valid: false, Errors: make([]parseErrorDetail, len(perrs.Errors))}
	for i, e := range perrs.Errors {
		resp.Errors[i] = parseErrorDetail{
			File:    e.File,
			Line:    e.Line,
			Column:  e.Column,
			Message: e.Message,
			Hint:    e.Hint,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

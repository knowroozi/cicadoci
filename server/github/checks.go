// Package github provides the GitHub-integration half of CICaDo's Forge
// adapter: the Checks API reporter and installation-token minting.
// It is deliberately transport-agnostic for the reporter — all
// it asks of a concrete implementation is the ability to create and
// update check runs given a run identifier.
//
// ChecksReporterAdapter (forge_adapter.go) is what actually satisfies
// server/forge's Reporter interface — the concrete ChecksClient type
// doesn't, on its own, since Go interface satisfaction is nominal and
// its RunRef/RerunRef/CheckConclusion/Annotation types are distinct
// from forge's (CIC-67). InstallationTokenMinter is used directly by
// runnersvc.CloneMinterFunc at lease time (CIC-151).
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// CheckStatus is the GitHub Checks API lifecycle state.
type CheckStatus string

const (
	// StatusQueued means "run accepted, waiting for a runner".
	StatusQueued CheckStatus = "queued"
	// StatusInProgress means "a runner is executing one of this check's jobs".
	StatusInProgress CheckStatus = "in_progress"
	// StatusCompleted means "all jobs finished; pass/fail determined by conclusion".
	StatusCompleted CheckStatus = "completed"
)

// CheckConclusion is the terminal quality of a check.
type CheckConclusion string

const (
	ConclusionSuccess        CheckConclusion = "success"
	ConclusionFailure        CheckConclusion = "failure"
	ConclusionSkipped        CheckConclusion = "skipped"
	ConclusionNeutral        CheckConclusion = "neutral"
	ConclusionActionRequired CheckConclusion = "action_required"
	ConclusionCancelled      CheckConclusion = "cancelled"
)

// Annotation is a single GitHub Checks API annotation attached to a
// check run failure.
type Annotation struct {
	Path      string `json:"path"`
	Line      int    `json:"start_line,omitempty"`
	Column    int    `json:"start_column,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	EndColumn int    `json:"end_column,omitempty"`
	Anchor    string `json:"anchor,omitempty"`
	Message   string `json:"message"`
	Title     string `json:"title,omitempty"`
	Level     string `json:"level"`
}

// createCheckRunRequest is the JSON body for POST /repos/{owner}/{repo}/check-runs.
type createCheckRunRequest struct {
	Name        string          `json:"name"`
	HeadSHA     string          `json:"head_sha"`
	HeadBranch  string          `json:"head_branch,omitempty"`
	Status      CheckStatus     `json:"status"`
	Conclusion  CheckConclusion `json:"conclusion,omitempty"`
	DetailsURL  string          `json:"details_url,omitempty"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	Annotations []Annotation    `json:"annotations,omitempty"`
}

// updateCheckRunRequest is the JSON body for PATCH /repos/{owner}/{repo}/check-runs/{id}.
type updateCheckRunRequest struct {
	Status      CheckStatus     `json:"status,omitempty"`
	Conclusion  CheckConclusion `json:"conclusion,omitempty"`
	DetailsURL  string          `json:"details_url,omitempty"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	Annotations []Annotation    `json:"annotations,omitempty"`
}

// RunRef identifies a run that needs its GitHub check updated.
type RunRef struct {
	Org        string
	Repo       string
	Sha        string
	RunID      string
	Branch     string
	Started    *time.Time
	Finished   *time.Time
	DetailsURL string
}

// RerunRef identifies a check rerun request.
type RerunRef struct {
	Org       string
	Repo      string
	Sha       string
	Branch    string
	CheckName string
}

// ChecksReporter is the seam that the rest of the server uses to report
// run/job status to a forge. CIC-28 will fold this into the Forge
// interface.
type ChecksReporter interface {
	// Prepare creates a check run for a run in the "queued" state.
	Prepare(ctx context.Context, run RunRef) (checkID int64, err error)
	// Start updates the check run to "in_progress".
	Start(ctx context.Context, checkID int64, run RunRef) error
	// Complete updates the check run to "completed" with a conclusion.
	Complete(ctx context.Context, checkID int64, run RunRef, conclusion CheckConclusion, annotations []Annotation) error
	// Rerun requests a new check run; called when GitHub's "Rerun checks"
	// button is pressed.
	Rerun(ctx context.Context, ref RerunRef) (runID string, err error)
}

// ChecksClient is the GitHub REST API implementation of ChecksReporter.
type ChecksClient struct {
	BaseURL    string
	HTTPClient *http.Client
	// AuthFunc returns a Bearer token for each request.
	AuthFunc func(ctx context.Context, ref RunRef) (token string, err error)
	Now      func() time.Time
}

// NewChecksClient returns a ChecksClient pointed at the live GitHub API.
func NewChecksClient(token string, httpClient *http.Client) *ChecksClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &ChecksClient{
		BaseURL:    "https://api.github.com",
		HTTPClient: httpClient,
		AuthFunc:   func(_ context.Context, _ RunRef) (string, error) { return token, nil },
		Now:        time.Now,
	}
}

func (c *ChecksClient) do(ctx context.Context, ref RunRef, method, path string, body any) (*http.Response, error) {
	// Build the body first so NewRequestWithContext can populate
	// ContentLength and GetBody for free (it type-switches on
	// *bytes.Reader). A nil bodyReader for GET-like requests is
	// handled by NewRequestWithContext.
	var bodyReader io.Reader
	if body != nil {
		reqBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(reqBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	if c.AuthFunc != nil {
		tok, err := c.AuthFunc(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("auth: %w", err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Prepare creates a new check run in the "queued" state.
//
//nolint:bodyclose // body is decoded, not just discarded.
func (c *ChecksClient) Prepare(ctx context.Context, ref RunRef) (int64, error) {
	req := createCheckRunRequest{
		Name:       "cicado",
		HeadSHA:    ref.Sha,
		HeadBranch: ref.Branch,
		Status:     StatusQueued,
		DetailsURL: ref.DetailsURL,
	}
	resp, err := c.do(ctx, ref, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/check-runs", ref.Org, ref.Repo), req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck
	if resp.StatusCode != http.StatusCreated {
		return 0, fmt.Errorf("createCheckRun: status %d: %s", resp.StatusCode, readBodySnippet(resp))
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// Start updates a check run to "in_progress".
func (c *ChecksClient) Start(ctx context.Context, checkID int64, ref RunRef) error {
	now := c.Now()
	req := updateCheckRunRequest{
		Status:    StatusInProgress,
		StartedAt: &now,
	}
	resp, err := c.do(ctx, ref, http.MethodPatch,
		fmt.Sprintf("/repos/%s/%s/check-runs/%d", ref.Org, ref.Repo, checkID), req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("updateCheckRun start: status %d: %s", resp.StatusCode, readBodySnippet(resp))
	}
	return nil
}

// Complete updates a check run to "completed" with a conclusion and
// optional annotations.
func (c *ChecksClient) Complete(ctx context.Context, checkID int64, ref RunRef, conclusion CheckConclusion, annotations []Annotation) error {
	now := c.Now()
	req := updateCheckRunRequest{
		Status:      StatusCompleted,
		Conclusion:  conclusion,
		CompletedAt: &now,
		Annotations: annotations,
	}
	resp, err := c.do(ctx, ref, http.MethodPatch,
		fmt.Sprintf("/repos/%s/%s/check-runs/%d", ref.Org, ref.Repo, checkID), req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("updateCheckRun complete: status %d: %s", resp.StatusCode, readBodySnippet(resp))
	}
	return nil
}

// Rerun is a stub; the full rerun flow is wired in CIC-24's receiver.
func (c *ChecksClient) Rerun(_ context.Context, _ RerunRef) (string, error) {
	return "", nil
}

// readBodySnippet reads up to 512 bytes from a response body for
// inclusion in error messages. It does not close the body; callers that
// own the body still defer its Close().
func readBodySnippet(resp *http.Response) string {
	const max = 512
	buf, err := io.ReadAll(io.LimitReader(resp.Body, max))
	if err != nil {
		return "<unreadable>"
	}
	if len(buf) == 0 {
		return ""
	}
	return string(buf)
}

var (
	_ ChecksReporter = (*ChecksClient)(nil)
)

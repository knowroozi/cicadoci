// Package github adds the GitHub-specific forge plumbing used by CICaDo's
// webhook and check-reporting paths.  This file is CIC-84: a workflow-file
// fetcher that reads `.github/workflows/*.y*ml` from a GitHub repo at a
// specific commit SHA, enabling production workflow producers.
package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// WorkflowFile is a single workflow file fetched from a GitHub repo.
type WorkflowFile struct {
	// Path is the repo-relative path, e.g. ".github/workflows/ci.yml".
	Path string
	// Content is the decoded YAML source.
	Content []byte
}

// WorkflowFetcher fetches workflow files from a GitHub repo at a given ref.
//
// WorkflowFetcher is not concurrent-safe; do not share a fetcher between
// goroutines.  Each FetchWorkflowFiles call is safe to call once per run.
type WorkflowFetcher struct {
	BaseURL    string
	HTTPClient *http.Client
	// BearerToken is the access token used for the /contents/ API calls.
	BearerToken string
}

// NewWorkflowFetcher returns a WorkflowFetcher pointed at the live GitHub API.
// When client is nil, a new default client is used.
func NewWorkflowFetcher(token string, client *http.Client) *WorkflowFetcher {
	if client == nil {
		client = http.DefaultClient
	}
	return &WorkflowFetcher{
		BaseURL:     githubAPIBaseURL,
		HTTPClient:  client,
		BearerToken: token,
	}
}

// githubContentsListEntry is one item in the directory-listing response.
// When the path points to a file, this does NOT include "content" — content
// is only populated when you fetch the file's own URL.
//
// See https://docs.github.com/en/rest/repos/contents#get-repository-contents
type githubContentsListEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"` // "dir" or "file"
	SHA         string `json:"sha"`
	Size        int    `json:"size"`
	DownloadURL string `json:"download_url"` // populated only when type=="file"
}

// githubContentsFileResponse is the response from fetching a single file.
//
// "Content" is base64-encoded YAML.
type githubContentsFileResponse struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	Content     string `json:"content"` // base64-encoded YAML (only for single-file mode)
	SHA         string `json:"sha"`
	DownloadURL string `json:"download_url"`
}

// FetchWorkflowFiles fetches all workflow YAML files in
// .github/workflows at the given ref (SHA or branch) from a GitHub repo,
// returning decoded content for each.
//
// It runs two HTTP requests per file: first a directory listing to get
// the file paths, then a per-file GET to retrieve each file's base64
// content.  This is necessary because the directory-listing endpoint
// (GET /repos/.../contents/.github/workflows) does not include the
// "content" field — that only appears in the per-file endpoint
// (GET /repos/.../contents/.github/workflows/ci.yaml).
//
// A 404 on the directory listing returns ErrWorkflowDirNotFound: GitHub
// answers 404 both when the directory is absent and when the token cannot
// read the repo (private repos outside a fine-grained PAT's selection), so
// the caller must not read it as "the repo has no workflows".  A 404 on a
// per-file request is not an error: the file was removed between the
// listing and the fetch.
//
// The ref is a commit SHA (from a push event) or a branch name.
func (f *WorkflowFetcher) FetchWorkflowFiles(
	ctx context.Context, owner, repo, ref string,
) ([]WorkflowFile, error) {
	// Phase 1: list the .github/workflows directory.
	dirURL := fmt.Sprintf(
		"%s/repos/%s/%s/contents/.github/workflows?ref=%s",
		f.BaseURL, url.PathEscape(owner), url.PathEscape(repo),
		url.QueryEscape(ref),
	)
	listing, err := f.getList(ctx, dirURL)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrWorkflowDirNotFound
		}
		return nil, fmt.Errorf("fetch workflow dir: %w", err)
	}

	// Phase 2: fetch each YAML file's content individually.
	var out []WorkflowFile
	for _, entry := range listing {
		if entry.Type != "file" {
			continue // skip sub-directories
		}
		if !strings.HasSuffix(entry.Name, ".yaml") && !strings.HasSuffix(entry.Name, ".yml") {
			continue // skip non-YAML files
		}

		fileURL := fmt.Sprintf(
			"%s/repos/%s/%s/contents/%s?ref=%s",
			f.BaseURL, url.PathEscape(owner), url.PathEscape(repo),
			url.PathEscape(entry.Path), url.QueryEscape(ref),
		)
		file, err := f.getFile(ctx, fileURL)
		if err != nil {
			if isNotFound(err) {
				continue // file removed between listing and fetch
			}
			return nil, fmt.Errorf("fetch workflow file %q: %w", entry.Path, err)
		}
		if file.Content == "" {
			continue // empty file
		}
		decoded, err := base64.StdEncoding.DecodeString(file.Content)
		if err != nil {
			return nil, fmt.Errorf("decode content of %q: %w", entry.Path, err)
		}
		out = append(out, WorkflowFile{Path: entry.Path, Content: decoded})
	}
	return out, nil
}

// getList fetches the JSON array response from a directory-listing endpoint
// and decodes it into a slice.  A 404 returns errNotFound.
func (f *WorkflowFetcher) getList(ctx context.Context, rawURL string) ([]githubContentsListEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	f.authHeaders(req)
	resp, err := f.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // body close on defer is not actionable

	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	var items []githubContentsListEntry
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, fmt.Errorf("decode dir listing: %w", err)
	}
	return items, nil
}

// getFile fetches the JSON response from a single-file endpoint and decodes
// it into a file response.  A 404 returns errNotFound.
func (f *WorkflowFetcher) getFile(ctx context.Context, rawURL string) (*githubContentsFileResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	f.authHeaders(req)
	resp, err := f.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // body close on defer is not actionable

	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	var file githubContentsFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&file); err != nil {
		return nil, fmt.Errorf("decode file response: %w", err)
	}
	return &file, nil
}

// authHeaders adds the Bearer Authorization header to a request when a
// token is configured.
func (f *WorkflowFetcher) authHeaders(req *http.Request) {
	if f.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+f.BearerToken)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
}

// ErrWorkflowDirNotFound is returned by FetchWorkflowFiles when the
// .github/workflows listing is a 404. It is ambiguous: the directory may be
// absent, or the repo, ref or token's access may be missing.
var ErrWorkflowDirNotFound = errors.New("github: .github/workflows not found or not readable")

var errNotFound = fmt.Errorf("not found")

func isNotFound(err error) bool {
	return err == errNotFound //nolint:errorlint // sentinel is a package-private value
}

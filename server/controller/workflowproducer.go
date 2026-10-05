// Package controller: workflow producer (CIC-84).
//
// The WorkflowProducer fetches workflow YAML files from a code-hosting
// provider, parses each with server/workflow/parse, and upserts the
// parsed definition into the store.
//
// It is called once per run, between CreateRun and LoadAndCreateJobs,
// so that the job DAG built from the store loader sees freshly-fetched
// workflows.  A nil producer is a no-op: the controller uses whatever
// workflows are already in the store (the dev-seed path).
//
// CIC-180: GitHubWorkflowProducer now supports per-call installation
// token minting (MinterFunc). When the GitHub App path is configured
// (--github-app-id + --github-app-key-file), each Produce mints a
// short-lived installation access token via github.InstallationTokenMinter,
// resolving the per-repo installation ID from repos.installation_id
// (stamped by CIC-146 P2 / PR #131).
//
// #89 provides store.UpsertWorkflow(ctx, store.WorkflowInput{...}). The
// producer uses that method via the narrow store.Interface it needs.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/github"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/workflow/parse"
)

// ── types ────────────────────────────────────────────────────────────────────

// WorkflowProducer fetches and persists workflow YAML for a repo.
//
// nil-safe: the controller treats a nil producer as "no producer configured"
// and skips the fetch, relying on the store loader.
type WorkflowProducer interface {
	// Produce fetches workflow YAML for owner/repo at ref (SHA or branch),
	// parses each file, and upserts it. When the workflow directory 404s,
	// Produce returns nil without error and leaves stored rows untouched.
	Produce(ctx context.Context, owner, repo, ref string, repoID uuid.UUID) error
}

// MinterFunc mints a GitHub installation access token for a specific repo
// on demand. It is called once per Produce call.
//
// Returns ("", nil) when the repo has no GitHub App installation
// (no GitHub App installation for this repo).
//
// The function must be safe for concurrent calls.
type MinterFunc func(ctx context.Context, repoID string) (string, error)

// GitHubWorkflowProducer is the GitHub-backed WorkflowProducer.
//
// It fetches .cicado/workflows/*.yaml at a given ref, parses each file
// with server/workflow/parse, and upserts the JSON-encoded workflow.
//
// Token source:
//   - When MinterFunc is non-nil, a fresh per-installation token is minted
//     per call (CIC-180 / CIC-146). See github.InstallationTokenMinter.
//     The HTTPClient field is used to construct the per-call fetcher.
//   - Otherwise a static BearerToken fetcher (fetcher) is reused (CIC-120:
//     a long-lived fine-grained PAT, suitable for short-lived dev servers).
type GitHubWorkflowProducer struct {
	fetcher      *github.WorkflowFetcher
	httpClient   *http.Client
	store        store.WorkflowStore
	log          *slog.Logger
	MaxFetchTime time.Duration
	// MinterFunc, when non-nil, mints a fresh GitHub installation
	// access token per Produce call using per-repo installation IDs.
	MinterFunc MinterFunc
}

// NewGitHubWorkflowProducer creates a producer that fetches from GitHub
// using a static Bearer token (CIC-120). A GitHub App installation token
// expires after ~1 h, so this constructor is for environments that
// configure a long-lived fine-grained PAT. Per-call token minting is
// available via NewGitHubWorkflowProducerWithMinter (CIC-180).
//
// httpClient may be nil (a default client is used).
func NewGitHubWorkflowProducer(
	token string,
	st store.WorkflowStore,
	log *slog.Logger,
	httpClient *http.Client,
) *GitHubWorkflowProducer {
	if log == nil {
		log = slog.Default()
	}
	return &GitHubWorkflowProducer{
		fetcher:      github.NewWorkflowFetcher(token, httpClient),
		httpClient:   httpClient,
		store:        st,
		log:          log,
		MaxFetchTime: 15 * time.Second,
	}
}

// NewGitHubWorkflowProducerWithMinter creates a producer that mints a
// fresh GitHub App installation access token per Produce call via
// minterFunc. The per-repo installation ID is resolved by minterFunc
// using repos.installation_id (stamped by CIC-146 P2 / PR #131).
//
// A new github.WorkflowFetcher is constructed for each call (the
// minter may return a different token each time), using httpClient.
//
// httpClient may be nil (a default client is used).
func NewGitHubWorkflowProducerWithMinter(
	minterFunc MinterFunc,
	st store.WorkflowStore,
	log *slog.Logger,
	httpClient *http.Client,
) *GitHubWorkflowProducer {
	if log == nil {
		log = slog.Default()
	}
	return &GitHubWorkflowProducer{
		httpClient:   httpClient,
		store:        st,
		log:          log,
		MaxFetchTime: 15 * time.Second,
		MinterFunc:   minterFunc,
	}
}

// ── Produce ───────────────────────────────────────────────────────────────────

// Produce fetches, parses, and upserts workflow YAML for the given repo.
//
// Token resolution:
//   - If MinterFunc is non-nil, a fresh installation token is minted per
//     call and a new WorkflowFetcher is constructed for that token.
//   - If MinterFunc is nil and fetcher is non-nil, the static-token fetcher
//     is reused (CIC-120 PAT path).
//   - If both MinterFunc and fetcher are nil, Produce is a no-op.
//
// A 404 at .cicado/workflows/ returns nil and changes nothing (no prune).
// A parse error on one file is logged but does not abort the others.
// ref should be the SHA from the push event. Empty ref => "main".
func (p *GitHubWorkflowProducer) Produce(
	ctx context.Context,
	owner, repo, ref string,
	repoID uuid.UUID,
) error {
	if p.store == nil {
		return nil
	}
	// Caller responsibility: ref must be the commit SHA from the
	// push event. A fallback to "main" would fetch the latest master
	// regardless of the actual ref, so we skip rather than silently
	// using the wrong content.
	if ref == "" {
		p.log.Warn("producer: empty ref; skipping — caller must pass a SHA",
			"owner", owner, "repo", repo)
		return nil
	}
	if p.MaxFetchTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.MaxFetchTime)
		defer cancel()
	}

	// ── resolve token ───────────────────────────────────────────────────
	var fetcher *github.WorkflowFetcher

	if p.MinterFunc != nil {
		// CIC-180: mint a fresh per-call installation token.
		token, mErr := p.MinterFunc(ctx, repoID.String())
		if mErr != nil {
			p.log.Warn("producer: token mint failed; skipping",
				"owner", owner, "repo", repo, "err", mErr)
			return nil
		}
		if token == "" {
			// No GitHub App installation for this repo: skip fetch, keep existing.
			p.log.Debug("producer: no installation token for repo; keeping existing",
				"owner", owner, "repo", repo)
			return nil
		}
		// Construct a fresh fetcher with the minted token and stored
		// HTTPClient (so tests can redirect to a local httptest server).
		fetcher = github.NewWorkflowFetcher(token, p.httpClient)
		p.log.Debug("producer: minted installation token",
			"owner", owner, "repo", repo, "ref", ref)
	} else if p.fetcher != nil {
		// CIC-120: use the static-token fetcher.
		fetcher = p.fetcher
	} else {
		return nil
	}

	files, err := fetcher.FetchWorkflowFiles(ctx, owner, repo, ref)
	// A listing 404 is not proof the repo has no workflows: GitHub also
	// returns 404 for a private repo the token cannot read. Pruning here
	// would delete every row for the repo, so leave the rows alone.
	if errors.Is(err, github.ErrWorkflowDirNotFound) {
		p.log.Info("producer: workflow dir not found or not readable; keeping existing workflows",
			"owner", owner, "repo", repo, "ref", ref)
		return nil
	}
	if err != nil {
		p.log.Warn("producer: fetch failed; skipping",
			"owner", owner, "repo", repo, "ref", ref, "err", err)
		return nil
	}

	// CIC-120: prune rows for workflow files that are gone at this ref, so
	// a file deleted from the repo stops triggering runs. The listing was
	// read (not a 404), so files is the complete set present at the ref.
	// Every fetched path is kept, including files that fail to parse below:
	// the file still exists and its last good definition stays until it
	// parses again or the next fetch at a new ref prunes it.
	present := make([]string, 0, len(files))
	for _, f := range files {
		present = append(present, f.Path)
	}
	if n, err := p.store.DeleteWorkflowsNotIn(ctx, repoID, present); err != nil {
		p.log.Warn("producer: prune of removed workflows failed",
			"owner", owner, "repo", repo, "ref", ref, "err", err)
	} else if n > 0 {
		p.log.Info("producer: removed workflows no longer in the repo",
			"owner", owner, "repo", repo, "ref", ref, "deleted", n)
	}
	if len(files) == 0 {
		p.log.Debug("producer: no workflow files at ref",
			"owner", owner, "repo", repo, "ref", ref)
		return nil
	}

	inserted := 0
	for _, wfFile := range files {
		wf, perr, parseErr := parse.Parse(wfFile.Content, wfFile.Path)
		if parseErr != nil {
			p.log.Warn("producer: parse failure; skipping",
				"owner", owner, "repo", repo, "path", wfFile.Path,
				"n_errors", len(perr.Errors))
			continue
		}
		jsonBytes, err := json.Marshal(wf)
		if err != nil {
			p.log.Warn("producer: marshal failure; skipping",
				"owner", owner, "repo", repo, "path", wfFile.Path, "err", err)
			continue
		}
		in := store.WorkflowInput{
			RepoID:     repoID,
			Name:       workflowNameFromPath(wfFile.Path),
			Path:       wfFile.Path,
			Source:     string(wfFile.Content),
			Definition: jsonBytes,
			ContentSHA: ref, // use the fetch ref as content_sha for dedup (CIC-120)
		}
		if _, err := p.store.UpsertWorkflow(ctx, in); err != nil {
			p.log.Warn("producer: upsert failure; skipping",
				"owner", owner, "repo", repo, "path", wfFile.Path, "err", err)
			continue
		}
		inserted++
		p.log.Debug("producer: upserted workflow", "path", wfFile.Path, "name", in.Name)
	}
	p.log.Info("producer: upserted workflow files",
		"owner", owner, "repo", repo, "ref", ref,
		"inserted", inserted, "total", len(files))
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// workflowNameFromPath extracts a short name from a GitHub workflow path.
// ".cicado/workflows/ci.yaml" → "ci", ".cicado/workflows/deploy.yml" → "deploy".
func workflowNameFromPath(path string) string {
	name := path
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' {
			name = name[i+1:]
			break
		}
	}
	switch {
	case len(name) >= 5 && name[len(name)-5:] == ".yaml":
		return name[:len(name)-5]
	case len(name) >= 4 && name[len(name)-4:] == ".yml":
		return name[:len(name)-4]
	}
	return name
}

// compile-time: *store.Store satisfies WorkflowStore.
var _ store.WorkflowStore = (*store.Store)(nil)

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
// #89 provides store.UpsertWorkflow(ctx, store.WorkflowInput{...}). The
// producer uses that method via the narrow store.Interface it needs.
package controller

import (
	"context"
	"encoding/json"
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
	// parses each file, and upserts it.  When no files are found at ref,
	// Produce returns nil without error.
	Produce(ctx context.Context, owner, repo, ref string, repoID uuid.UUID) error
}

// GitHubWorkflowProducer is the GitHub-backed WorkflowProducer.
//
// It uses a github.WorkflowFetcher with a Bearer token to fetch
// .github/workflows/*.yaml at the given ref, parses each file with
// server/workflow/parse, and upserts the JSON-encoded workflow.
type GitHubWorkflowProducer struct {
	fetcher      *github.WorkflowFetcher
	store        store.WorkflowStore
	log          *slog.Logger
	MaxFetchTime time.Duration
}

// NewGitHubWorkflowProducer creates a producer that fetches from GitHub
// using the given installation access token.
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
		store:        st,
		log:          log,
		MaxFetchTime: 15 * time.Second,
	}
}

// ── Produce ───────────────────────────────────────────────────────────────────

// Produce fetches, parses, and upserts workflow YAML for the given repo.
//
// A 404 at .github/workflows/ means "no workflows yet"; returns nil.
// A parse error on one file is logged but does not abort the others.
// ref should be the SHA from the push event.  Empty ref => "main".
func (p *GitHubWorkflowProducer) Produce(
	ctx context.Context,
	owner, repo, ref string,
	repoID uuid.UUID,
) error {
	if p.fetcher == nil || p.store == nil {
		return nil
	}
	if ref == "" {
		// Caller responsibility: ref must be the commit SHA from the
		// push event.  A fallback to "main" would fetch the latest
		// master regardless of the actual ref, so we log a warning and
		// skip rather than silently using the wrong content.
		p.log.Warn("producer: empty ref; skipping — caller must pass a SHA",
			"owner", owner, "repo", repo)
		return nil
	}
	if p.MaxFetchTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.MaxFetchTime)
		defer cancel()
	}

	files, err := p.fetcher.FetchWorkflowFiles(ctx, owner, repo, ref)
	if err != nil {
		p.log.Warn("producer: fetch failed; skipping",
			"owner", owner, "repo", repo, "ref", ref, "err", err)
		return nil
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
			ContentSHA: ref, // use the fetch ref as content_sha for dedup
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
// ".github/workflows/ci.yaml" → "ci", ".github/workflows/deploy.yml" → "deploy".
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

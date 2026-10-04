// clone_minter.go: CIC-151 / CIC-146 — per-job GitHub installation token
// minting for the github_clone_token secret ref.
//
// The installation ID is resolved per-repo from repos.installation_id
// (set at App-install time by the webhook receiver or by `cicado migrate
// install` for existing installations).
//
// Fork PR runs get a contents:read installation token; this is the minimal
// permission needed to clone the fork's default branch. All other secret
// refs are refused for fork runs by GetJobSecrets (CIC-27 defense-in-depth).
//
// The token is NEVER persisted to the database. Its lifetime is bounded by
// GitHub (default 1 h) and the mint is scoped to the specific installation.
package runnersvc

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"cicadoci.com/cicado/server/github"
	"cicadoci.com/cicado/server/redact"
)

// ── Options ───────────────────────────────────────────────────────────────

// NewCloneMinterFuncOption configures minter construction.
type NewCloneMinterFuncOption func(*minterOpts)

// minterOpts holds optional fields for NewCloneMinterFunc.
type minterOpts struct {
	// BaseURL overrides the GitHub API base; used by httptest-based tests.
	BaseURL string
	// HTTPClient overrides the default http.Client; used by httptest-based
	// tests that point a real httptest.NewServer at the closure.
	HTTPClient *http.Client
}

// WithBaseURL returns a NewCloneMinterFuncOption that sets the GitHub API
// base URL.  Used by tests; production callers (main.go) leave this empty.
func WithBaseURL(baseURL string) NewCloneMinterFuncOption {
	return func(o *minterOpts) { o.BaseURL = baseURL }
}

// WithHTTPClient returns a NewCloneMinterFuncOption that sets the
// http.Client.  Used by tests with httptest.NewServer; production callers
// leave this nil (the default is a 10s-timeout client).
func WithHTTPClient(client *http.Client) NewCloneMinterFuncOption {
	return func(o *minterOpts) { o.HTTPClient = client }
}

// ── CloneMinterFunc type ──────────────────────────────────────────────────

// CloneMinterFunc mints a per-job, per-run GitHub installation token on
// demand. It is called by the gRPC GetJobSecrets handler when
// secret_refs includes "github_clone_token".
//
// The installation ID is resolved per-repo (from repos.installation_id),
// so each job gets a token scoped to its own repo's installation.
//
// isFork: true for fork PR runs. The minter still returns a
// contents:read-scoped token in that case (the fork's own repo can be
// cloned); GetJobSecrets refuses all other secrets for fork runs.
//
// Returns a short-lived installation token and any error. The token
// is NEVER persisted and is registered with the supplied
// redact.RegisteringMasker before being returned to the caller, so
// that any log payload carrying the token value is scrubbed
// (CIC-74 invariant).
//
// Returns ("", nil) when the repo has no installation_id set — this is
// an *anonymous clone* (public/unauthenticated git clone); NOT a
// configuration error.
//
// Returns nil when appID == 0 or keyBytes is empty (no minter wired).
// main.go only wires the minter when both are configured.
type CloneMinterFunc func(ctx context.Context, orgID, repoID string, isFork bool) (string, error)

// clonePermissionsRead is the GitHub permission scope used for clone
// tokens. "read" is the minimum privilege needed for `git clone`.
const clonePermissionsRead = "read"

// ── Builder ───────────────────────────────────────────────────────────────

// NewCloneMinterFunc builds a CloneMinterFunc that resolves the
// installation ID from the repos table and mints a GitHub installation
// token via github.InstallationTokenMinter.
//
// The returned function is nil when appID == 0, keyBytes is empty, or
// pool is nil — main.go only wires the minter when all three are set.
//
// The resolver maps a CICaDo repo UUID to (installationID, repoName).
// installation_id is set at App-install time by the webhook receiver
// or by `cicado migrate install`.  repoName is used for the B2 per-repo
// scoping in the GitHub API request body.
//
// B5: when raw installation_id is empty or 0 (no App install configured
// for this repo), the minter returns ("", nil) — anonymous clone.  A
// hard error is returned only for real failures (auth, HTTP, decode).
//
// The opts are variadic; production main.go callers pass none.  Tests
// pass WithBaseURL and WithHTTPClient so that the HTTP request body
// reaches a local httptest server, proving the B2 per-repo scoping
// through the *real* closure.
func NewCloneMinterFunc(
	ctx context.Context,
	pool *pgxpool.Pool,
	appID int64,
	keyBytes []byte,
	masker redact.RegisteringMasker,
	opts ...NewCloneMinterFuncOption,
) CloneMinterFunc {
	if appID == 0 || len(keyBytes) == 0 || pool == nil {
		return nil
	}
	var m minterOpts
	for _, o := range opts {
		o(&m)
	}
	cfg := github.InstallationConfig{
		AppID:         appID,
		AppPrivateKey: keyBytes,
		// CIC-151 / conv-e2490b #2: contents:read is the
		// minimal permission for a git clone; fork and
		// non-fork runs both get this scope.
		ClonePermissions: clonePermissionsRead,
		// B4a: optional httptest overrides.
		BaseURL:    m.BaseURL,
		HTTPClient: m.HTTPClient,
	}
	minter := &github.InstallationTokenMinter{
		Config: cfg,
		Auth:   github.AppAuthFunc,
		Masker: masker,
	}
	// resolver maps a CICaDo repo UUID to (installationID, repoName).
	// installation_id is set at App-install time by the webhook
	// receiver or by `cicado migrate install` for existing installations.
	// B5: coalesce gives '' for NULL; we treat '' and "0" as "not set".
	resolver := func(reqCtx context.Context, repoID string) (int64, string, error) {
		var raw, repoName string
		err := pool.QueryRow(reqCtx,
			`SELECT coalesce(installation_id, ''), name FROM repos WHERE id = $1`,
			repoID).Scan(&raw, &repoName)
		if err != nil {
			return 0, "", fmt.Errorf("clone minter: look up installation id: %w", err)
		}
		if raw == "" {
			// B5: no installation => anonymous clone; caller omits the token.
			return 0, "", nil
		}
		var id int64
		_, err = fmt.Sscanf(raw, "%d", &id)
		if err != nil {
			return 0, "", fmt.Errorf("clone minter: parse installation id %q: %w", raw, err)
		}
		if id == 0 {
			// B5: "0" is the "not set" sentinel in the DB.
			return 0, "", nil
		}
		return id, repoName, nil
	}
	return func(reqCtx context.Context, _orgID, repoID string, isFork bool) (string, error) {
		installationID, repoName, err := resolver(reqCtx, repoID)
		if err != nil {
			return "", err
		}
		// B5: no installation_id means anonymous clone.
		if installationID == 0 {
			return "", nil
		}
		// B2: pass repoName so the token is scoped to this specific
		// repository, not the entire installation.
		token, err := mintCloneToken(reqCtx, minter, installationID, repoName)
		if err != nil {
			return "", err
		}
		return token, nil
	}
}

// mintCloneToken delegates to github.InstallationTokenMinter.Mint.
//
// It is called when installationID is non-zero (a real installation
// exists for this repo).  The token is scoped to the given installation
// and repo.  Both fork and non-fork runs get a contents:read-scoped
// token; GetJobSecrets enforces that fork runs get no static-secret
// values.
func mintCloneToken(
	ctx context.Context,
	minter *github.InstallationTokenMinter,
	installationID int64,
	repoName string,
) (string, error) {
	// B2: pass repoName so the token is scoped to this repo.
	token, err := minter.Mint(ctx, installationID, "", repoName)
	if err != nil {
		return "", fmt.Errorf("clone minter: %w", err)
	}
	return token, nil
}

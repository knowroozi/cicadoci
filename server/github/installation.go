package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"cicadoci.com/cicado/server/redact"
)

// InstallationConfig holds the static parameters needed to mint a
// per-job installation access token via the GitHub API.
//
// AppID + AppPrivateKey can be passed to a token Minter; the minter
// creates a short-lived JWT for the "app → installation" leg, then
// exchanges it for an installation access token that is scoped to a
// single repo and lives long enough for one git clone.
//
// The AppPrivateKey is the GitHub App's private key (PEM-encoded RSA
// or Ed25519). It is NEVER persisted in the database, NEVER logged, and
// NEVER transmitted to runners: it lives only in the server process
// (loaded once from a mounted secret file or environment variable).
type InstallationConfig struct {
	// AppID is the numeric GitHub App ID.
	AppID int64
	// AppPrivateKey is the PEM-encoded private key.
	// Loaded from a mounted file or env var at startup; never stored.
	AppPrivateKey []byte
	// Expiry is the installation token lifetime. GitHub allows up to 1 hour.
	// Default: 10 minutes (sufficient for a clone + checkout).
	Expiry time.Duration
	// Now is overridable in tests; defaults to time.Now.
	Now func() time.Time
	// HTTPClient is nilable; a default 10s-timeout client is created when nil.
	HTTPClient *http.Client
	// BaseURL overrides the GitHub API base (for testing).
	BaseURL string
	// ClonePermissions is the permission scope for the GitHub installation
	// token. "read" or "write" (maps to GitHub's permissions[].content).
	// Default: "read" (CIC-151: a clone token needs only contents:read).
	// Empty string means no permissions field is sent (all-installation scope).
	ClonePermissions string
}

// DefaultExpiry is the default installation-token lifetime: 10 minutes.
const DefaultExpiry = 10 * time.Minute

// InstallationTokenMinter mints short-lived GitHub installation access
// tokens for per-job git clone. Each token is scoped to a single
// installation and never persisted.
type InstallationTokenMinter struct {
	Config InstallationConfig
	// Auth is the JWT-authentication function: given an app ID + key,
	// produce a Bearer token for the /app/installations/{id}/access_tokens
	// endpoint. In production this is a short-lived RS256/EdDSA JWT
	// signed with AppPrivateKey and verified by GitHub.
	//
	// The MVP implementation is a stub that returns a static token.
	// The full JWT implementation is CIC-26+ (a small follow-up).
	Auth func(ctx context.Context, cfg InstallationConfig) (string, error)

	// Masker receives every freshly-minted installation token via
	// RegisterWithVariants before the token reaches a git clone URL.
	//
	// MUST be non-nil in production. A nil Masker means the token is
	// NOT registered for log redaction — this was the CIC-74 gap:
	// RegisterWithVariants existed but was called nowhere, so installation
	// tokens leaked into logs via failed git-clone error output.
	//
	// Use redact.NewRegisteredMasker() for the production default.
	Masker redact.RegisteringMasker
}

// InstallationTokenResponse is a subset of GitHub's
// POST /app/installations/{id}/access_tokens response.
type installationTokenResponse struct {
	Token string `json:"token"`
	// Expiration is RFC3339.
	Expiration string `json:"expiration"`
}

// Mint creates a new installation access token for the given
// installation and repo. The returned token is suitable for embedding
// in a clone URL: https://x-access-token:TOKEN@github.com/org/repo.git
//
// The token is NEVER persisted: the caller must ensure it doesn't end
// up in a log, the database, or a job record's clone_url field
// in plaintext. Registration with the redaction masker (CIC-31) is
// the caller's responsibility.
//
// The token is valid for cfg.Expiry (default 10 min) and is scoped by
// GitHub to the installation it was minted from plus the specified
// repository permissions.
func (m *InstallationTokenMinter) Mint(ctx context.Context,
	installationID int64, org, repo string) (string, error) {

	cfg := m.Config
	if cfg.Expiry == 0 {
		cfg.Expiry = DefaultExpiry
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.github.com"
	}

	// Step 1: get an auth token (app-level JWT or similar).
	auth, err := m.auth(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("installation auth: %w", err)
	}

	// Step 2: POST /app/installations/{id}/access_tokens.
	//
	// We scope the token to the single repo this job needs by passing
	// "repositories": [repo]. Without this, GitHub grants the token
	// access to every repo the installation can see — defeating the
	// least-privilege premise of per-job installation tokens. See
	// PR #25 review (inline on installation.go:104).
	reqBody := map[string]any{
		tkExpiry: cfg.Now().Add(cfg.Expiry).UTC().Format("2006-01-02T15:04:05Z"),
	}
	if repo != "" {
		reqBody[tkPerRepo] = []string{repo}
	}
	// CIC-151: scope the installation token to contents:read by default,
	// or contents:write when explicitly requested. Both fork and non-fork
	// clone paths call Mint with ClonePermissions="read".
	if perm := cfg.ClonePermissions; perm != "" {
		reqBody[tkPermissions] = map[string]string{"contents": perm}
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/app/installations/%d/access_tokens",
			cfg.BaseURL, installationID),
		bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("mint token http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("mint token: status %d (org=%s repo=%s)",
			resp.StatusCode, org, repo)
	}

	var out installationTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode mint response: %w", err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("mint token: empty token in response")
	}
	// Note: we deliberately DO NOT log the token here.

	// Register with the log-redaction masker so that if this token ever
	// leaks into log output (e.g. a git-clone error that prints the full
	// URL) it is scrubbed on ingest. CIC-74: without this, RegisterWithVariants
	// had zero non-test call sites and installation tokens could appear in
	// raw log files, violating the project's "secrets never in logs" invariant.
	if m.Masker != nil {
		m.Masker.RegisterWithVariants(out.Token)
	}
	return out.Token, nil
}

// auth delegates to the configured Auth function.
//
// An unconfigured minter returns an error rather than a fake token,
// so an operator who forgets to wire Auth sees a clear
// "Auth not configured" message at the call site instead of a
// confusing 401 from GitHub's real API. See PR #25 review.
func (m *InstallationTokenMinter) auth(ctx context.Context, cfg InstallationConfig) (string, error) {
	if m.Auth == nil {
		return "", fmt.Errorf("installation auth: Auth function not configured")
	}
	return m.Auth(ctx, cfg)
}

// JSON key constants for installation token request.
const (
	tkPerRepo     = "repositories" // GitHub expects "repositories" not "per_repo"
	tkExpiry      = "expires_at"
	tkPermissions = "permissions" // CIC-151: contents permission scope (read/write)
)

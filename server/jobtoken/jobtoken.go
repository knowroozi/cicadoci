// Package jobtoken mints, stores, and verifies the per-job ephemeral
// platform tokens (CIC-40) that the runner holds while a job runs.
//
// A job token is a short-lived bearer credential, scoped to the
// specific (org, run, job) it was minted for and to an optional
// branch, plus a set of API scopes. A job uses it to talk to the
// CICaDo platform API — fetch logs, push status, list or download
// artifacts — and only within the run it was scoped to. After the run
// ends (or the token is revoked, or it expires, whichever comes
// first) the token has no power.
//
// Storage and minting are separate so the caller gets the token
// plaintext exactly once, at mint time, and never again. The store
// keeps only the SHA-256 hash of the token; the hash is the lookup
// key, so the token itself never crosses into a durable store. The
// invariant, enforced by the store itself, is that a job token is
// not recoverable from the database — only its hash is.
//
// Every issue, use, and revoke is audited through the Auditor the
// manager is built with. The invariant "every state-changing action
// writes an audit event" (CLAUDE.md) is the reason the Auditor is a
// first-class seam: a manager without an Auditor is legal only in
// tests.
//
// A job token is NOT an OIDC token. OIDC is CIC-39: a JWT the server
// signs so a cloud platform can assume a role from a job's identity.
// A job token is an unguessable random bearer scoped to a run and
// held against the platform for the life of the run — no trust
// boundary crosses on it, so a 256-bit random beats a signed token
// (no long-lived key material on the hot path, no signature to
// forge, one less thing to reason about in a security review).
package jobtoken

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/auth"
)

// Scope is what the token may do on the platform API. Scopes are
// additive: a token that has ScopeWrite also has ScopeRead.
//
// MVP exposes two scopes. A fork-PR job is restricted to ScopeRead
// (ADR-forkpolicy, docs); non-fork jobs get both. The fork decision
// is enforced at the mint site, not inside the token: this package
// is agnostic to the policy and reasons only about the value.
type Scope string

const (
	// ScopeRead lets a job read runs, logs, the workflow file, and
	// list or download artifacts for its own run. It is the
	// baseline for every job token, including fork-PR jobs.
	ScopeRead Scope = "read"

	// ScopeWrite permits state-changing operations the job may
	// perform within its own run: report step output, update job
	// status, upload an artifact, request a cancel. It is NOT
	// granted to fork-PR jobs per ADR-forkpolicy.
	ScopeWrite Scope = "write"
)

// hasScope reports whether s is in the list.
func hasScope(list []Scope, s Scope) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// WithRead returns scopes with ScopeRead ensured present.
func WithRead(scopes ...Scope) []Scope {
	if hasScope(scopes, ScopeRead) {
		return scopes
	}
	return append([]Scope{ScopeRead}, scopes...)
}

// WithWrite returns scopes with ScopeRead and ScopeWrite ensured
// present. Use it for a non-fork job that needs to push status.
func WithWrite(scopes ...Scope) []Scope {
	if hasScope(scopes, ScopeWrite) {
		return WithRead(scopes...)
	}
	return append([]Scope{ScopeRead, ScopeWrite}, scopes...)
}

// Claims are the run-scoped bounds a job token carries. A verifier
// returns these to the caller when it presents a valid token so the
// caller can authorize within the run; the API does NOT trust
// anything in the request body that contradicts the token's claims.
type Claims struct {
	// OrgID is the org the run belongs to. Every API call the token
	// authorizes is constrained to this org's rows.
	OrgID uuid.UUID

	// RepoID is the repo the run belongs to.
	RepoID uuid.UUID

	// RunID is the run the token is bound to. A job may not reach
	// any run other than this one with this token.
	RunID uuid.UUID

	// JobID is the job the token is bound to. A job's own
	// lifecycle actions are scoped to this job.
	JobID uuid.UUID

	// Branch is the branch the run is on (a free-form label; for
	// fork PRs this is the fork owner's branch, not the base).
	// Used by the API for branch-specific policy (e.g. refuse to
	// deploy on an untrusted branch).
	Branch string

	// Issuer is the principal that minted the token — a human
	// (actor=user:xyz), a system component (actor=controller,
	// actor=webhook), or an agent session (actor=agent:xyz).
	// Recorded to the audit entry for traceability.
	Issuer string

	// Scopes are the API scopes the token grants.
	Scopes []Scope
}

// Validate ensures the claims are structurally complete. A misformed
// claim record is a bug in a caller, not a security incident.
func (c Claims) Validate() error {
	if c.OrgID == uuid.Nil {
		return errors.New("jobtoken: claims missing org id")
	}
	if c.RunID == uuid.Nil {
		return errors.New("jobtoken: claims missing run id")
	}
	if c.JobID == uuid.Nil {
		return errors.New("jobtoken: claims missing job id")
	}
	if c.RepoID == uuid.Nil {
		return errors.New("jobtoken: claims missing repo id")
	}
	if len(c.Scopes) == 0 {
		return errors.New("jobtoken: claims must request at least one scope")
	}
	return nil
}

// Token is a platform token record. Raw holds the plaintext bearer:
// it is populated only at Issue time, returned exactly once, and
// never written to storage by this package.
//
// Do not retain past the moment of issuance. The runner process
// picks the value up from the lease envelope, uses it to talk to the
// platform API for the duration of the run, and drops it when the
// run ends.
//
// The remaining fields describe what the token authorizes, when it
// expires, and its internal identifier (used for the Revoke path and
// the audit trail).
type Token struct {
	// Raw is the plaintext bearer, returned once at Issue time.
	// The store never sees it — only its SHA-256 hash.
	Raw string

	// ID is the internal, opaque identifier. Use it, not Raw, when
	// referring to the token in the audit log or in a Revoke call.
	ID uuid.UUID

	// Claims are the run-scoped bounds the token enforces.
	Claims Claims

	// Expiry is when the token stops being valid, absolutely. A
	// Verify call after this time returns ErrExpired.
	Expiry time.Time

	// IssuedAt is when the token was minted, absolutely.
	IssuedAt time.Time
}

// HasScope reports whether the token grants scope s.
func (t Token) HasScope(s Scope) bool {
	return hasScope(t.Claims.Scopes, s)
}

// record is the durable on-disk row. It never holds the raw bearer,
// only its hash.
type record struct {
	ID        uuid.UUID
	Hash      []byte
	Claims    Claims
	Expiry    time.Time
	IssuedAt  time.Time
	RevokedAt *time.Time
}

// IsRevoked reports whether the record has been revoked.
func (r record) IsRevoked() bool { return r.RevokedAt != nil }

// Store is the token record store.
//
// Implementations must hash the raw bearer at Mint time, store only
// the hash, and never return the resolved raw.
type Store interface {
	// Mint stores a new record for hash(raw) with the given claims
	// and expiry, and returns the record's ID.
	Mint(ctx context.Context, raw string, c Claims, expiry time.Time) (uuid.UUID, error)

	// Lookup returns the stored record for a hash, or
	// ErrStoreNotFound if none matches.
	Lookup(ctx context.Context, hash []byte) (record, error)

	// MarkRevoked sets the record's RevokedAt to now if it is not
	// already revoked, and returns changed=true if a transition
	// actually happened. The auditor fires only on a real transition,
	// not on a no-op re-revoke, so the interface distinguishes the
	// two.
	MarkRevoked(ctx context.Context, id uuid.UUID, at time.Time) (changed bool, err error)

	// SetLastUse records the most recent use time of the record. A
	// no-op if the record does not exist.
	SetLastUse(ctx context.Context, id uuid.UUID, when time.Time) error
}

// Auditor is a pluggable event sink. Each call fires at a distinct
// lifecycle event.
//
// A nil Auditor is rejected by NewManager (fail-closed): the invariant
// "every state change writes an audit event" is load-bearing.
// Use NewMemoryAuditor in tests; NewAuditLog (audit.go) in production.
type Auditor interface {
	// Issue fires when a new token is minted.
	Issue(ctx context.Context, t Token) error

	// Use fires when a token is successfully verified on its
	// happy path.
	Use(ctx context.Context, t Token) error

	// Revoke fires when a token is revoked.
	Revoke(ctx context.Context, id uuid.UUID, at time.Time) error
}

// MemoryAuditor is an in-memory Auditor for tests.
type MemoryAuditor struct {
	Issues []Token
	Uses   []Token
	Revocs []RevocEvent
}

// RevocEvent is the (ID, when) pair recorded by Revoke.
type RevocEvent struct {
	ID uuid.UUID
	At time.Time
}

// NewMemoryAuditor creates an in-memory Auditor.
func NewMemoryAuditor() *MemoryAuditor { return &MemoryAuditor{} }

func (m *MemoryAuditor) Issue(_ context.Context, t Token) error {
	m.Issues = append(m.Issues, t)
	return nil
}

func (m *MemoryAuditor) Use(_ context.Context, t Token) error {
	m.Uses = append(m.Uses, t)
	return nil
}

func (m *MemoryAuditor) Revoke(_ context.Context, id uuid.UUID, at time.Time) error {
	m.Revocs = append(m.Revocs, RevocEvent{ID: id, At: at})
	return nil
}

// Manager mints, verifies, and revokes job tokens.
//
// A Manager holds a Store (durable record) and an optional Auditor
// (event sink). The Manager does the bearer generation, lookup,
// expiry/revocation checks, and audit dispatch; the Store handles
// persistence; the Auditor handles logging.
//
// A Manager is safe for concurrent use; the safety comes from the
// store and auditor.
type Manager struct {
	store   Store
	auditor Auditor // required; see NewManager
	clock   func() time.Time
	log     *slog.Logger
}

// NewManager builds a Manager from a Store. The auditor is required and
// must not be nil: CLAUDE.md names "every state-changing action writes an
// audit event" as a security invariant, and both minting and revoking a
// credential are state changes. A nil auditor would silently drop every
// audit, so the constructor fails closed. Pass NewMemoryAuditor for tests
// that need a sink without a sink of their own.
func NewManager(s Store, a Auditor) (*Manager, error) {
	return newManager(s, a, time.Now)
}

// NewManagerWithClock is NewManager with an injected clock, used for
// deterministic expiry tests.
func NewManagerWithClock(s Store, a Auditor, clock func() time.Time) (*Manager, error) {
	if clock == nil {
		clock = time.Now
	}
	return newManager(s, a, clock)
}

func newManager(s Store, a Auditor, clock func() time.Time) (*Manager, error) {
	if a == nil {
		return nil, errors.New("jobtoken: an auditor is required")
	}
	return &Manager{store: s, auditor: a, clock: clock, log: slog.Default()}, nil
}

// Issue mints a job token for claims with the given ttl.
//
// The raw bearer is generated via auth.Generate(auth.KindJob), stored
// by hash only, recorded to the auditor, and returned as the
// token's Raw field. The raw is not stored. The caller shows it to
// the job's holder once and then drops it; only the record ID
// remains.
//
// The manager's clock() is the "now" reference: IssuedAt is
// clock(), Expiry is clock() + ttl.
//
// TTL must be strictly positive. A zero or negative TTL is a
// caller bug — the "ephemeral" invariant is that the token
// expires, so an "infinite" TTL is illegal. The Manager rejects it
// with ErrNonPositiveTTL.
func (m *Manager) Issue(ctx context.Context, claims Claims, ttl time.Duration) (Token, error) {
	if err := claims.Validate(); err != nil {
		return Token{}, err
	}
	if ttl <= 0 {
		return Token{}, ErrNonPositiveTTL
	}
	raw, err := auth.Generate(auth.KindJob)
	if err != nil {
		return Token{}, fmt.Errorf("jobtoken: generate: %w", err)
	}
	now := m.clock()
	expiry := now.Add(ttl)
	id, err := m.store.Mint(ctx, raw, claims, expiry)
	if err != nil {
		return Token{}, fmt.Errorf("jobtoken: mint: %w", err)
	}
	tok := Token{Raw: raw, ID: id, Claims: claims, Expiry: expiry, IssuedAt: now}
	if err := m.auditor.Issue(ctx, tok); err != nil {
		// A failing audit is a system fault, not a token fault: the token
		// is already minted and usable. Don't block the job on it, but don't
		// silently discard the failure either — log it at Warn with the token
		// id so a real audit outage is visible in the logs.
		m.log.Warn("jobtoken: issue audit failed", "token_id", tok.ID, "err", err)
	}
	return tok, nil
}

// Verify looks up the presented raw by hash, checks it is usable
// (not revoked, not expired), and — when scope is set — checks the
// token grants that scope.
//
// On success the token is returned with Raw populated (so a caller
// can reuse it downstream). The use is recorded to the auditor and
// the store's last-used timestamp.
//
// On any failure the returned error is one of the sentinels below.
// Callers at the API boundary should collapse all of them to one
// "invalid credential" response: distinguishing them in the
// response leaks which path the attacker chose and enables
// enumeration.
func (m *Manager) Verify(ctx context.Context, raw string, scope Scope) (Token, error) {
	raw = stripBearer(raw)
	if err := auth.Validate(auth.KindJob, raw); err != nil {
		return Token{}, ErrMalformed
	}
	h := sha256.Sum256([]byte(raw))
	rec, err := m.store.Lookup(ctx, h[:])
	if err != nil {
		return Token{}, errStore(err)
	}
	if rec.IsRevoked() {
		return Token{}, ErrRevoked
	}
	now := m.clock()
	if now.After(rec.Expiry) {
		return Token{}, ErrExpired
	}
	// A blank scope is a fail-open footgun: a caller that omits the argument
	// or passes a zero-value Scope would otherwise slip past the
	// authorization check. Reject it so an unscoped call fails closed.
	if scope == "" {
		return Token{}, ErrForbidden
	}
	if !hasScope(rec.Claims.Scopes, scope) {
		return Token{}, ErrForbidden
	}
	tok := Token{
		ID:       rec.ID,
		Claims:   rec.Claims,
		Expiry:   rec.Expiry,
		IssuedAt: rec.IssuedAt,
		Raw:      raw,
	}
	_ = m.store.SetLastUse(ctx, rec.ID, now)
	if err := m.auditor.Use(ctx, tok); err != nil {
		m.log.Warn("jobtoken: use audit failed", "token_id", tok.ID, "err", err)
	}
	return tok, nil
}

// Revoke marks the token id revoked. It is idempotent. The token
// stops verifying at the next Verify call (any Verify that happens
// after this call and after the record commit fails with
// ErrRevoked).
func (m *Manager) Revoke(ctx context.Context, id uuid.UUID) error {
	at := m.clock()
	changed, err := m.store.MarkRevoked(ctx, id, at)
	if err != nil {
		return err
	}
	// Audit only a real transition; a no-op re-revoke is not a
	// state change.
	if changed {
		if err := m.auditor.Revoke(ctx, id, at); err != nil {
			m.log.Warn("jobtoken: revoke audit failed", "token_id", id, "at", at, "err", err)
		}
	}
	return nil
}

// stripBearer peels a "Bearer " or "bearer " prefix that HTTP
// callers commonly pass. It is a no-op on a bare token and on
// anything shorter than six characters.
func stripBearer(s string) string {
	for _, p := range []string{"Bearer ", "bearer "} {
		if len(s) >= len(p) && s[:len(p)] == p {
			return s[len(p):]
		}
	}
	return s
}

// errStore maps store errors to the package sentinels.
//
// The store must not leak "revoked" as a distinct error before
// Verify checks it: a revoked token and a not-found token are
// indistinguishable to callers. The revocation check happens AFTER
// the store returns the record, so a store error that means "no
// such hash" collapses to ErrNotFound here.
func errStore(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrStoreNotFound) {
		return ErrNotFound
	}
	// Anything the store returns that isn't "no such record" is a system
	// fault (a DB outage, a bad query). It must not be reported as a missing
	// token, or logs and metrics collapse an outage into "bad token" and a
	// failed DB call looks like a caller mistake. Wrap so the cause survives.
	return fmt.Errorf("jobtoken: store: %w", err)
}

// Sentinel errors. The API layer collapses all of them to a single
// "bad credential" response; the Manager exposes them so internal
// logging can name the failure mode precisely.
//
// Each of the four failure classes below is distinct at the
// Manager level but collapses to the same HTTP status and the same
// body at the HTTP API boundary.
var (
	// ErrMalformed is returned when the presented raw token is not
	// well-formed (bad kind prefix, bad length, bad base64,
	// etc.). The caller never sees a token that isn't ours.
	ErrMalformed = errors.New("jobtoken: malformed token")

	// ErrNotFound is returned when the presented token is
	// well-formed but no record with its hash is stored.
	ErrNotFound = errors.New("jobtoken: token not found")

	// ErrExpired is returned when a valid record exists but the
	// clock has passed its expiry.
	ErrExpired = errors.New("jobtoken: token expired")

	// ErrRevoked is returned when a valid, unexpired record exists
	// but has been revoked through Revoke.
	ErrRevoked = errors.New("jobtoken: token revoked")

	// ErrForbidden is returned when a valid, unexpired, unrevoked
	// token does not carry the requested scope.
	ErrForbidden = errors.New("jobtoken: token lacks requested scope")

	// ErrNonPositiveTTL is returned when Issue is called with a
	// ttl <= 0. The ephemeral invariant is that tokens must
	// expire; a non-positive ttl defeats that.
	ErrNonPositiveTTL = errors.New("jobtoken: ttl must be positive")

	// ErrStoreNotFound is the store-level "no such record" sentinel.
	// It is unexported; callers only see ErrNotFound through
	// errStore, never this one directly.
	ErrStoreNotFound = errors.New("jobtoken: store: no such record")
)

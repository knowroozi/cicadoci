package jobtoken

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"cicadoci.com/cicado/server/auth"
)

// Postgres is a Store backed by the tokens table.
//
// The tokens table (server/store/migrations/0001_init.sql) carries
// the fields a job token needs: a token_hash bytea (unique), the
// subject columns (org_id, repo_id, run_id, job_id all allowed —
// a job token sets all of them), a jsonb claims column, an
// expires_at, and a revoked_at. The raw bearer is never written;
// only its SHA-256 hash is.
//
// Postgres is the production backend; Memory is for tests and
// single-process deployments.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres creates a Postgres Store from a pool. The pool must
// already be connected and its schema migrated so the tokens table
// exists.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

// claimsJSON is the on-disk form of Claims. It is the subset of
// Claims that the API needs to enforce at request time.
type claimsJSON struct {
	RepoID string   `json:"repo_id,omitempty"`
	RunID  string   `json:"run_id"`
	JobID  string   `json:"job_id"`
	OrgID  string   `json:"org_id"`
	Branch string   `json:"branch,omitempty"`
	Issuer string   `json:"issuer,omitempty"`
	Scopes []string `json:"scopes"`
}

// claimsToJSON serializes Claims to the canonical JSON form stored
// in the tokens.claims column. The Marshal of a fixed-shape struct
// cannot fail; a panic is a bug in this package, not a recoverable
// input error.
func claimsToJSON(c Claims) []byte {
	j := claimsJSON{
		RepoID: c.RepoID.String(),
		RunID:  c.RunID.String(),
		JobID:  c.JobID.String(),
		OrgID:  c.OrgID.String(),
		Branch: c.Branch,
		Issuer: c.Issuer,
		Scopes: scopesToStrings(c.Scopes),
	}
	b, err := json.Marshal(j)
	if err != nil {
		panic("jobtoken: claims marshal: " + err.Error())
	}
	return b
}

// claimsFromJSON deserializes claims stored in a tokens row.
func claimsFromJSON(b []byte) (Claims, error) {
	var j claimsJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return Claims{}, err
	}
	claims := Claims{Branch: j.Branch, Issuer: j.Issuer}
	for _, s := range j.Scopes {
		claims.Scopes = append(claims.Scopes, Scope(s))
	}
	if j.RunID != "" {
		u, err := uuid.Parse(j.RunID)
		if err != nil {
			return Claims{}, err
		}
		claims.RunID = u
	}
	if j.JobID != "" {
		u, err := uuid.Parse(j.JobID)
		if err != nil {
			return Claims{}, err
		}
		claims.JobID = u
	}
	if j.OrgID != "" {
		u, err := uuid.Parse(j.OrgID)
		if err != nil {
			return Claims{}, err
		}
		claims.OrgID = u
	}
	if j.RepoID != "" {
		u, err := uuid.Parse(j.RepoID)
		if err != nil {
			return Claims{}, err
		}
		claims.RepoID = u
	}
	return claims, nil
}

func scopesToStrings(scopes []Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}
	return out
}

// Mint inserts a job token record. The raw bearer is hashed
// locally and never sent to the database.
//
// max_uses is left to its default (1); job tokens are not
// consumed-on-use (Verify does not increment uses), so the column
// is irrelevant to enforcement. Writing 0 would violate the
// CHECK (max_uses > 0), so the column is omitted from the INSERT
// entirely.
func (p *Postgres) Mint(ctx context.Context, raw string, c Claims, expiry time.Time) (uuid.UUID, error) {
	h := sha256.Sum256([]byte(raw))
	var id uuid.UUID
	err := p.pool.QueryRow(ctx, `
INSERT INTO tokens (
    kind, token_hash, org_id, repo_id, run_id, job_id,
    claims, expires_at, created_by
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9
)
RETURNING id`,
		string(auth.KindJob),
		h[:],
		c.OrgID,
		c.RepoID,
		c.RunID,
		c.JobID,
		claimsToJSON(c),
		expiry,
		c.Issuer,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// Lookup finds the record by its hash. A missing hash returns
// ErrStoreNotFound so the Manager can collapse it to ErrNotFound.
func (p *Postgres) Lookup(ctx context.Context, hash []byte) (record, error) {
	var (
		id        uuid.UUID
		claimsB   []byte
		expiry    time.Time
		revokedAt *time.Time
		issuer    string
		issuedAt  time.Time
	)
	err := p.pool.QueryRow(ctx, `
SELECT id, claims, expires_at, revoked_at, created_by, created_at
FROM tokens
WHERE token_hash = $1 AND kind = 'job'
LIMIT 1`,
		hash,
	).Scan(&id, &claimsB, &expiry, &revokedAt, &issuer, &issuedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return record{}, ErrStoreNotFound
		}
		return record{}, err
	}
	claims, err := claimsFromJSON(claimsB)
	if err != nil {
		return record{}, err
	}
	// created_by is the authoritative issuer: it is set by the
	// caller and stored alongside the token, not derived.
	claims.Issuer = issuer
	return record{
		ID:        id,
		Hash:      append([]byte(nil), hash...),
		Claims:    claims,
		Expiry:    expiry,
		RevokedAt: revokedAt,
		IssuedAt:  issuedAt,
	}, nil
}

// MarkRevoked sets revoked_at on the matching record. It is
// idempotent: a row already revoked stays revoked and returns
// changed=false. A transition (revoked_at NULL → set) returns
// changed=true so the Manager can audit only real state changes.
func (p *Postgres) MarkRevoked(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
UPDATE tokens
SET revoked_at = $1
WHERE id = $2 AND kind = 'job' AND revoked_at IS NULL`,
		at, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetLastUse records the most recent use timestamp. A no-op (zero
// rows updated) for an unknown id.
func (p *Postgres) SetLastUse(ctx context.Context, id uuid.UUID, when time.Time) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE tokens SET last_used_at = $1 WHERE id = $2 AND kind = 'job'`,
		when, id)
	return err
}

// Ensure Postgres satisfies Store.
var _ Store = (*Postgres)(nil)

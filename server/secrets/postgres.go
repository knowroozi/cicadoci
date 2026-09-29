// Postgres is a Store implementation backed by the `secrets` table.
//
// It stores encrypted secret payloads (AES-256-GCM) keyed by
// (org_id, repo_id, name). Repo-scoped secrets (repo_id != NULL)
// take precedence over org-scoped secrets (scope='org'); the
// Get method tries the repo-level first, then falls back to the
// org-level.
//
// The encryption key is injected at construction. For production,
// an operator supplies a 32-byte key via CICADO_SECRET_KEY env
// (hex-encoded); KMS envelope encryption is a M9 concern.
//
// The `secrets` table schema (0001_init.sql) has a unique expression
// index:
//
//	CREATE UNIQUE INDEX secrets_scope_name_idx ON secrets (
//	    org_id,
//	    coalesce(repo_id, '00000000-0000-0000-0000-000000000000'::uuid),
//	    coalesce(environment, ''),
//	    name
//	   );
//
// Postgres does not support ON CONFLICT on expression indices, so Put
// uses a transactional SELECT-FOR-UPDATE-then-INSERT/UPDATE pattern instead.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	crand "crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cicadoci.com/cicado/server/store"
)

// gcmNonceSize is the AES-GCM fixed nonce length (96 bits).
const gcmNonceSize = 12

// Postgres implements Store against the secrets table.
type Postgres struct {
	store *store.Store
	key   EncryptionKey
	// expiry is the lease TTL returned by Get.
	expiry time.Duration
}

// NewPostgres creates a Postgres store with the given 32-byte key.
// The lease expiry defaults to 5 minutes.
func NewPostgres(s *store.Store, key EncryptionKey) *Postgres {
	return &Postgres{store: s, key: key, expiry: 5 * time.Minute}
}

// Put stores or overwrites a secret.
// orgID and repoID are UUID strings; an empty repoID stores an
// org-scoped secret (scope='org').
//
// The upsert is done in a transaction: the existing row is found and
// locked via FOR UPDATE (using the same coalesce expression the
// unique index enforces), then updated or a new row is inserted.
func (p *Postgres) Put(ctx context.Context, orgID, repoID, name string, value []byte, _ []string) error {
	oid, err := uuid.Parse(orgID)
	if err != nil {
		return fmt.Errorf("secrets: invalid org_id: %w", err)
	}
	if name == "" {
		return fmt.Errorf("secrets: name must not be empty")
	}

	var rid *uuid.UUID
	scope := "org"
	if repoID != "" {
		parsed, err := uuid.Parse(repoID)
		if err != nil {
			return fmt.Errorf("secrets: invalid repo_id: %w", err)
		}
		rid = &parsed
		scope = "repo"
	}

	ciphertext, err := p.seal(value)
	if err != nil {
		return err
	}

	tx, err := p.store.Pool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("secrets: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var existingID uuid.UUID
	if rid == nil {
		err = tx.QueryRow(ctx, `
			SELECT id FROM secrets
			 WHERE org_id = $1 AND repo_id IS NULL AND name = $2
			   AND coalesce(environment, '') = ''
			 ORDER BY version DESC LIMIT 1 FOR UPDATE`,
			oid, name).Scan(&existingID)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT id FROM secrets
			 WHERE org_id = $1 AND repo_id = $2 AND name = $3
			   AND coalesce(environment, '') = ''
			 ORDER BY version DESC LIMIT 1 FOR UPDATE`,
			oid, *rid, name).Scan(&existingID)
	}

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No existing row — insert.
		if _, err := tx.Exec(ctx, `
			INSERT INTO secrets
			     (org_id, repo_id, name, scope, ciphertext,
			 encrypted_data_key, kms_key_id, created_by, version)
			VALUES ($1, $2, $3, $4, $5, ''::bytea, '', 'system', 1)`,
			oid, rid, name, scope, ciphertext); err != nil {
			return fmt.Errorf("secrets: insert %q: %w", name, err)
		}
	case err != nil:
		return fmt.Errorf("secrets: lookup %q: %w", name, err)
	default:
		// Existing row found — update in place.
		if _, err := tx.Exec(ctx,
			`UPDATE secrets SET ciphertext = $1, updated_at = now() WHERE id = $2`,
			ciphertext, existingID); err != nil {
			return fmt.Errorf("secrets: update %q: %w", name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("secrets: commit %q: %w", name, err)
	}
	return nil
}

// Get returns a lease for the named secret.
// Tries repo-scoped first, then org-scoped.
func (p *Postgres) Get(ctx context.Context, orgID, repoID, name string) (*Lease, error) {
	oid, err := uuid.Parse(orgID)
	if err != nil {
		return nil, fmt.Errorf("secrets: invalid org_id: %w", err)
	}
	if name == "" {
		return nil, fmt.Errorf("secrets: name must not be empty")
	}

	// Repo-scoped attempt first when a repoID is given.
	if repoID != "" {
		rid, err := uuid.Parse(repoID)
		if err != nil {
			return nil, fmt.Errorf("secrets: invalid repo_id: %w", err)
		}
		var ct []byte
		err = p.store.Pool().QueryRow(ctx, `
			SELECT ciphertext FROM secrets
			 WHERE org_id = $1 AND repo_id = $2 AND name = $3
			   AND coalesce(environment, '') = ''
			 LIMIT 1`,
			oid, rid, name).Scan(&ct)
		if errors.Is(err, pgx.ErrNoRows) {
			// Fall through to org-scoped.
		} else if err != nil {
			return nil, fmt.Errorf("secrets: get repo-scoped %q: %w", name, err)
		} else {
			return p.makeLease(ct, name)
		}
	}

	// Org-scoped attempt (also serves as the fallback for repo-scoped misses).
	var ct []byte
	err = p.store.Pool().QueryRow(ctx, `
		SELECT ciphertext FROM secrets
		 WHERE org_id = $1 AND repo_id IS NULL AND name = $2
		   AND coalesce(environment, '') = ''
		LIMIT 1`,
		oid, name).Scan(&ct)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("secrets: not found: %q for org %q", name, orgID)
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: get org-scoped %q: %w", name, err)
	}
	return p.makeLease(ct, name)
}

func (p *Postgres) makeLease(ct []byte, name string) (*Lease, error) {
	plaintext, err := p.open(ct)
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt %q: %w", name, err)
	}
	buf := make([]byte, len(plaintext))
	copy(buf, plaintext)
	return &Lease{
		Value:  buf,
		Name:   name,
		Expiry: time.Now().Add(p.expiry),
	}, nil
}

// Delete removes a secret.
// When repoID is non-empty, deletes the repo-scoped row.
// When repoID is empty, deletes the org-scoped row.
func (p *Postgres) Delete(ctx context.Context, orgID, repoID, name string) error {
	oid, err := uuid.Parse(orgID)
	if err != nil {
		return fmt.Errorf("secrets: invalid org_id: %w", err)
	}
	if repoID != "" {
		rid, perr := uuid.Parse(repoID)
		if perr != nil {
			return fmt.Errorf("secrets: invalid repo_id: %w", perr)
		}
		_, perr = p.store.Pool().Exec(ctx,
			`DELETE FROM secrets WHERE org_id = $1 AND repo_id = $2 AND name = $3`,
			oid, rid, name)
		if perr != nil {
			return fmt.Errorf("secrets: delete %q: %w", name, perr)
		}
		return nil
	}
	if _, perr := p.store.Pool().Exec(ctx,
		`DELETE FROM secrets WHERE org_id = $1 AND repo_id IS NULL AND name = $2`,
		oid, name); perr != nil {
		return fmt.Errorf("secrets: delete %q: %w", name, perr)
	}
	return nil
}

// List returns secret names visible in the given org/repo scope.
// Org-scoped names always appear; repo-scoped names appear only
// when repoID is non-empty.
func (p *Postgres) List(ctx context.Context, orgID, repoID string) ([]string, error) {
	oid, err := uuid.Parse(orgID)
	if err != nil {
		return nil, fmt.Errorf("secrets: invalid org_id: %w", err)
	}

	seen := make(map[string]struct{})
	var result []string

	add := func(rows pgx.Rows) error {
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			if _, dup := seen[n]; !dup {
				seen[n] = struct{}{}
				result = append(result, n)
			}
		}
		return rows.Err()
	}

	// Org-scoped names.
	rows, err := p.store.Pool().Query(ctx,
		`SELECT name FROM secrets WHERE org_id = $1 AND repo_id IS NULL`, oid)
	if err != nil {
		return nil, err
	}
	if err := add(rows); err != nil {
		return nil, err
	}

	// Repo-scoped names.
	if repoID != "" {
		rid, perr := uuid.Parse(repoID)
		if perr != nil {
			return nil, fmt.Errorf("secrets: invalid repo_id: %w", perr)
		}
		rows2, err := p.store.Pool().Query(ctx,
			`SELECT name FROM secrets WHERE org_id = $1 AND repo_id = $2`,
			oid, rid)
		if err != nil {
			return nil, err
		}
		if err := add(rows2); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// ─── AES-256-GCM helpers ───────────────────────────────────────────────────

// seal encrypts plaintext; output is nonce||ciphertext||tag (tag appended by GCM).
func (p *Postgres) seal(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(p.key[:])
	if err != nil {
		return nil, fmt.Errorf("secrets: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: new gcm: %w", err)
	}
	nonce := make([]byte, gcmNonceSize)
	if _, err := crand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: generate nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, gcmNonceSize+len(ct))
	out = append(out, nonce...)
	return append(out, ct...), nil
}

// open decrypts a nonce||ciphertext||tag blob.
func (p *Postgres) open(blob []byte) ([]byte, error) {
	if len(blob) < gcmNonceSize {
		return nil, fmt.Errorf("secrets: ciphertext too short (%d bytes)", len(blob))
	}
	nonce := blob[:gcmNonceSize]
	ct := blob[gcmNonceSize:]
	block, err := aes.NewCipher(p.key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ct, nil)
}

// Compile-time assertion.
var _ Store = (*Postgres)(nil)

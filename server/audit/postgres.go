// Copyright 2024 Kaveh Nowroozi. Apache-2.0.
//
// PostgresLog is the Postgres-backed audit.Log for the audit_events table.
//
// The Entry hash chain:
//
//	hash_n = SHA256(prev_hash || action || actor || target || reason || ts)
//	prev_hash = hash_{n-1}
//
// The hash excludes ID (Postgres-assigned, unknowable at hash time).
// The prev_hash is stored in payload_hash (bytea); metadata jsonb
// carries only reason (human-readable, for query convenience).
package audit

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresLog is a pgx-backed implementation of the Log interface,
// persisting to the audit_events table created by migration 0001.
//
// It matches the pattern set by queue.Postgres (pgxpool.Pool, not
// *sql.DB), so tests use testdb.New(t) directly.
type PostgresLog struct {
	pool *pgxpool.Pool
}

// NewPostgresLog returns a PostgresLog backed by the given pool.
// The database must have migration 0001_init.sql applied (the
// audit_events table).
func NewPostgresLog(pool *pgxpool.Pool) *PostgresLog {
	return &PostgresLog{pool: pool}
}

var _ Log = (*PostgresLog)(nil)

const genesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Append inserts a new audit entry.
//
// The audit_events table uses bigserial for id; the returned int64
// is the Postgres-assigned id.
//
// Hash computation: e.ID is deliberately NOT included in the hash
// (see hashEntry doc-comment in audit.go). The id assigned by
// Postgres is not available at hash time; chain integrity is
// maintained by the prev_hash pointer.
func (p *PostgresLog) Append(ctx context.Context, e Entry) (int64, error) {
	prev, err := p.lastPrevHash(ctx)
	if err != nil {
		return 0, fmt.Errorf("audit: append: last prev_hash: %w", err)
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}

	// Postgres timestamptz truncates to microsecond precision.
	// Truncate before both the hash and the INSERT so the value that
	// gets hashed matches the value stored and later read back.
	e.Timestamp = e.Timestamp.Truncate(time.Microsecond)

	// Hash with the previous hash; e.ID is intentionally zero
	// and excluded from hashEntry.
	e.ID = 0
	e.PrevHash = prev
	e.Hash = hashEntry(e)

	// payload_hash: raw hash bytes (32 bytes = SHA-256).
	hb, err := hex.DecodeString(e.Hash)
	if err != nil {
		return 0, fmt.Errorf("audit: append: decode hash hex: %w", err)
	}

	// CIC-192: normalize unset principal metadata to the historical INSERT literal
	// so callers that predate this PR keep their stored-value behaviour. A blank
	// principal_type defaults to "user" (the old Append INSERT literal was
	// ('user', principal_type; 'system', surface; NULL, org_id) -- a single
	// unclassified user-on-one-surface; 'user' is the historical default) and a
	// blank surface to "system" (the safest label for an unclassified internal
	// write). A non-blank value is stored verbatim. These fields are NOT part of
	// the hash (hashEntry is stable -- see
	// docs/decisions/CIC-192-audit-principal-fidelity.md), so the defaults do
	// not affect tamper-evidence; only the stored row reflects them.
	if e.PrincipalType == "" {
		e.PrincipalType = "user"
	}
	if e.Surface == "" {
		e.Surface = "system"
	}

	// reason and prev_hash go into jsonb metadata.
	// NOTE: ::text casts are required because jsonb_build_object takes
	// `any` args and pgx's extended query protocol cannot infer types
	// without explicit casts.
	var id int64
	err = p.pool.QueryRow(ctx,
		`INSERT INTO audit_events
			(org_id, principal_type, principal_id, surface,
			 action, subject_type, subject_id,
			 payload_hash, metadata, occurred_at)
			 VALUES (NULLIF($1::text, '')::uuid, $2, $3, $4,
			$5, '', $6, $7,
			jsonb_build_object('reason', $8::text, 'prev_hash', $9::text),
			$10)
			RETURNING id`,
		e.OrgID, e.PrincipalType, e.Actor, e.Surface,
		string(e.Action), e.Target, hb, e.Reason, prev,
		e.Timestamp).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("audit: insert: %w", err)
	}
	return id, nil
}

// Get returns an entry by its Postgres-assigned id.
func (p *PostgresLog) Get(ctx context.Context, id int64) (Entry, error) {
	var (
		occurredAt time.Time
		principal  string
		action     string
		subjID     string
		payloadB   []byte
		reason     string
		prevHash   string
	)
	var orgID, principalType, surface string
	err := p.pool.QueryRow(ctx,
		`SELECT occurred_at, principal_id, action,
			coalesce(subject_id, ''), payload_hash,
			coalesce(metadata->>'reason', ''),
			coalesce(metadata->>'prev_hash', ''),
			coalesce(org_id::text, ''), coalesce(principal_type, ''), coalesce(surface, '')
		 FROM audit_events WHERE id = $1`,
		id).Scan(&occurredAt, &principal, &action,
		&subjID, &payloadB, &reason, &prevHash,
		&orgID, &principalType, &surface)
	if err != nil {
		// pgx returns pgx.ErrNoRows, not sql.ErrNoRows.
		return Entry{}, fmt.Errorf("audit: get: %w", err)
	}
	return Entry{
		ID:            id,
		Action:        Action(action),
		Actor:         principal,
		Target:        subjID,
		Reason:        reason,
		Timestamp:     occurredAt,
		Hash:          hex.EncodeToString(payloadB),
		PrevHash:      prevHash,
		OrgID:         orgID,
		PrincipalType: principalType,
		Surface:       surface,
	}, nil
}

// List returns entries whose id exceeds afterID, up to limit entries.
func (p *PostgresLog) List(ctx context.Context, afterID int64, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.pool.Query(ctx,
		`SELECT id, occurred_at, principal_id, action,
			coalesce(subject_id, ''),
			payload_hash,
			coalesce(metadata->>'reason', ''),
			coalesce(metadata->>'prev_hash', ''),
			coalesce(org_id::text, ''), coalesce(principal_type, ''), coalesce(surface, '')
			 FROM audit_events
			 WHERE id > $1 ORDER BY id ASC LIMIT $2`,
		afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("audit: list: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var (
			id        int64
			occurred  time.Time
			principal string
			action    string
			subjID    string
			payloadB  []byte
			reason    string
			prevHash  string
		)
		var orgID, principalType, surface string
		if err := rows.Scan(&id, &occurred, &principal, &action,
			&subjID, &payloadB, &reason, &prevHash, &orgID, &principalType, &surface); err != nil {
			return nil, fmt.Errorf("audit: list scan: %w", err)
		}
		out = append(out, Entry{
			ID:            id,
			Action:        Action(action),
			Actor:         principal,
			Target:        subjID,
			Reason:        reason,
			Timestamp:     occurred,
			Hash:          hex.EncodeToString(payloadB),
			PrevHash:      prevHash,
			OrgID:         orgID,
			PrincipalType: principalType,
			Surface:       surface,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: list: %w", err)
	}
	return out, nil
}

// VerifyChain re-derives the hash chain by reading all rows in id order
// and checking that each row's stored payload_hash matches the hash
// computed from its fields plus the previous row's hash.
func (p *PostgresLog) VerifyChain(ctx context.Context) error {
	rows, err := p.pool.Query(ctx,
		`SELECT id, occurred_at,
			coalesce(principal_id, ''),
			coalesce(action, ''),
			coalesce(subject_id, ''),
			coalesce(metadata->>'reason', ''),
			coalesce(metadata->>'prev_hash', ''),
			payload_hash
			 FROM audit_events
			 ORDER BY id ASC`)
	if err != nil {
		return fmt.Errorf("audit: verify: query: %w", err)
	}
	defer rows.Close()

	prevHash := genesisHash
	i := 0
	for rows.Next() {
		var (
			id         int64
			occurred   time.Time
			actor      string
			action     string
			subjID     string
			reason     string
			storedPrev string
			payloadB   []byte
		)
		if err := rows.Scan(&id, &occurred, &actor, &action,
			&subjID, &reason, &storedPrev, &payloadB); err != nil {
			return fmt.Errorf("audit: verify: scan: %w", err)
		}
		storedHash := hex.EncodeToString(payloadB)
		e := Entry{
			ID:        id, // intentionally excluded from hash
			Action:    Action(action),
			Actor:     actor,
			Target:    subjID,
			Reason:    reason,
			PrevHash:  prevHash,
			Timestamp: occurred,
		}
		computed := hashEntry(e)
		if computed != storedHash {
			return fmt.Errorf("audit: chain broken at entry %d (id=%d): computed=%s stored=%s",
				i, id, computed, storedHash)
		}
		// Also verify the stored prev_hash matches what we computed.
		if storedPrev != prevHash {
			return fmt.Errorf("audit: chain broken at entry %d (id=%d): stored_prev=%s expected=%s",
				i, id, storedPrev, prevHash)
		}
		prevHash = computed
		i++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("audit: verify: %w", err)
	}
	return nil
}

// lastPrevHash returns the hex-encoded payload_hash of the most-recent
// entry, which is that entry's actual hash (hash_n). The next Append
// chains off hash_n, not the row's stored prev_hash (which would be
// hash_{n-1} — too far back by one step).
//
// Returns genesisHash when the table is empty.
func (p *PostgresLog) lastPrevHash(ctx context.Context) (string, error) {
	var hashHex string
	err := p.pool.QueryRow(ctx,
		`SELECT coalesce(
			  (SELECT encode(payload_hash, 'hex')
			  FROM audit_events ORDER BY id DESC LIMIT 1),
			  $1
			)`,
		genesisHash).Scan(&hashHex)
	if err != nil {
		return "", err
	}
	if hashHex == "" {
		return genesisHash, nil
	}
	return hashHex, nil
}

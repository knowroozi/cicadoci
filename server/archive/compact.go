// Package archive compacts per-job log rows from Postgres (hot) into a
// single object in a cold BlobStore, and reads them back through a
// LogReader that falls through to the blob when the hot rows are gone.
//
// The compacted object is the system of record for completed jobs; the
// `log_archives` table holds its location and is what a read path
// consults first when the hot rows are not found.
//
// MVP format: a single JSON object with a version discriminator and an
// ordered array of entries. A future version can introduce a streamed
// framing (e.g. MessagePack or a custom binary) without breaking
// existing archives — readers should branch on `version`.
package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"cicadoci.com/cicado/server/blobstore"
)

// FormatVersion is the on-disk version of the compacted blob.
const FormatVersion = 1

// Entry is one log row in compacted form.
//
// It mirrors the log_entries schema. Chunk rows carry Data; event rows
// carry EventKind + EventJSON.
type Entry struct {
	Seq       int64  `json:"seq"`
	Kind      string `json:"kind"` // "chunk" | "event"
	StepIndex int32  `json:"step_index"`
	// Chunk fields (one or the other, depending on Kind).
	IO       string     `json:"io,omitempty"`
	Data     []byte     `json:"data,omitempty"`
	RunnerTS *time.Time `json:"runner_ts,omitempty"`
	// Event fields.
	EventKind string          `json:"event_kind,omitempty"`
	EventJSON json.RawMessage `json:"event_json,omitempty"`
}

// Blob is the compacted representation of one job's logs.
type Blob struct {
	Version int     `json:"version"`
	JobID   string  `json:"job_id"`
	Entries []Entry `json:"entries"`
}

// Marshal serializes a Blob for storage.
func (b Blob) Marshal() ([]byte, error) {
	if b.Version != FormatVersion {
		b.Version = FormatVersion
	}
	return json.Marshal(b)
}

// errNoArchive signals that a job has no archive row.
var errNoArchive = errors.New("archive: job not archived")

// Compactor moves a completed job's logs from Postgres into the
// BlobStore.
//
// It is safe to call concurrently for different job IDs. For the same
// job it must be called at most once — a second call returns the
// existing key without re-writing.
type Compactor struct {
	pool *pgxpool.Pool
	bs   blobstore.BlobStore
}

// NewCompactor builds a Compactor.
func NewCompactor(pool *pgxpool.Pool, bs blobstore.BlobStore) *Compactor {
	return &Compactor{pool: pool, bs: bs}
}

// Archive flushes a job's hot log rows to the BlobStore, prunes the
// hot rows, and writes a log_archives row. The returned key is the
// object key of the written blob.
//
// The steps are not atomic: if BlobStore.Put succeeds but the Postgres
// delete fails, the job's logs exist in two places. On retry the
// compactor detects the existing log_archives row and returns the
// existing key without re-writing.
func (c *Compactor) Archive(ctx context.Context, jobID uuid.UUID) (string, error) {
	// 1. Idempotency: if this job is already archived, return the key.
	if key, err := c.existingArchiveKey(ctx, jobID); err == nil {
		return key, nil
	}

	// 2. Read all hot rows in seq order, capturing the maximum seq we
	// saw so the subsequent prune can be bounded. A trailing chunk
	// (seq > maxSeq) that lands between the read and the delete is
	// left in hot and survives the archive — see recordArchiveAndPrune.
	rows, maxSeq, err := c.readAllHot(ctx, jobID)
	if err != nil {
		return "", fmt.Errorf("read hot logs: %w", err)
	}

	// 3. Compact + write to the blob store.
	blob := Blob{
		Version: FormatVersion,
		JobID:   jobID.String(),
		Entries: rows,
	}
	data, err := blob.Marshal()
	if err != nil {
		return "", fmt.Errorf("marshal blob: %w", err)
	}
	key := fmt.Sprintf("logs/%s.v%d.json", jobID.String(), FormatVersion)
	if err := c.bs.Put(ctx, key, data); err != nil {
		return "", fmt.Errorf("put blob: %w", err)
	}

	// 4. Record the archive + prune the hot rows in one transaction.
	// Pass maxSeq so the prune is bounded; a trailing chunk that
	// arrived after the read is not silently lost.
	if err := c.recordArchiveAndPrune(ctx, jobID, key, maxSeq, int64(len(rows)), int64(len(data))); err != nil {
		return "", fmt.Errorf("record archive: %w", err)
	}
	return key, nil
}

// ReadAll returns every log entry for a job, with read-through to the
// blob when the hot rows are pruned.
//
// A job with no logs returns an empty slice.
func (c *Compactor) ReadAll(ctx context.Context, jobID uuid.UUID) ([]Entry, error) {
	// 1. Hot rows first — live job, or a job whose archive write
	// succeeded but prune has not yet completed.
	// We ignore the maxSe return here: ReadAll only needs the rows.
	rows, _, err := c.readAllHot(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		return rows, nil
	}
	// 2. Hot rows are gone: fall through to the blob.
	key, err := c.existingArchiveKey(ctx, jobID)
	if errors.Is(err, errNoArchive) {
		return nil, nil // job exists, no logs at all
	}
	if err != nil {
		return nil, err
	}
	data, err := c.bs.Get(ctx, key)
	if err != nil {
		// Blob is missing but log_archives said it existed: a
		// storage-layer failure. Surface it.
		return nil, fmt.Errorf("read blob %q: %w", key, err)
	}
	var out Blob
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse blob %q: %w", key, err)
	}
	// Defensive sort: the writer produces ordered output but a future
	// writer may not, and the read path must stay stable.
	sort.Slice(out.Entries, func(i, j int) bool {
		return out.Entries[i].Seq < out.Entries[j].Seq
	})
	return out.Entries, nil
}

// existingArchiveKey returns the object key for jobID's archive.
//
// The error errNoArchive is a sentinel meaning "no archive row exists".
// Callers should treat that as "this job is not archived yet" rather
// than a hard error.
func (c *Compactor) existingArchiveKey(ctx context.Context, jobID uuid.UUID) (string, error) {
	var key string
	err := c.pool.QueryRow(ctx,
		`SELECT object_key FROM log_archives WHERE job_id = $1`, jobID).
		Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNoArchive
	}
	if err != nil {
		return "", err
	}
	return key, nil
}

// readAllHot reads every hot row for a job in seq order.
//
// Returns (nil, 0, nil) when no rows exist — a live job that has not
// yet logged anything.
func (c *Compactor) readAllHot(ctx context.Context, jobID uuid.UUID) ([]Entry, int64, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT seq, kind, step_index,
			COALESCE(io, ''), data, runner_ts,
			COALESCE(event_kind, ''), event_json
		FROM log_entries
		WHERE job_id = $1
		ORDER BY seq`, jobID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []Entry
	var maxSeq int64
	for rows.Next() {
		var (
			seq       int64
			kind      string
			stepIdx   int32
			io        string
			data      []byte
			ts        *time.Time
			eventKind string
			eventJSON []byte
		)
		if err := rows.Scan(&seq, &kind, &stepIdx,
			&io, &data, &ts, &eventKind, &eventJSON); err != nil {
			return nil, 0, err
		}
		out = append(out, Entry{
			Seq:       seq,
			Kind:      kind,
			StepIndex: stepIdx,
			IO:        io,
			Data:      data,
			RunnerTS:  ts,
			EventKind: eventKind,
			EventJSON: json.RawMessage(eventJSON),
		})
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, maxSeq, nil
}

// recordArchiveAndPrune writes the log_archives row and prunes the
// hot rows (bounded by maxSeq) in one transaction.
//
// It deliberately deletes only rows with seq <= maxSeq. An
// unconditional `DELETE ... WHERE job_id = $1` would silently drop a
// trailing chunk that landed on the write path between the
// readAllHot call and this delete — it would not be in the blob
// (it post-dates the read) and would not be in hot (the delete
// removed it). This is the "read + delete atomic" gap flagged in
// PR #31's review.
func (c *Compactor) recordArchiveAndPrune(ctx context.Context, jobID uuid.UUID, key string, maxSeq, count, bytes int64) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	// On any non-happy exit roll back; a successful path commits first,
	// which makes this a no-op.
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // tx closed by Commit on happy path
	// Prune only the rows we just archived. A row that landed after
	// the read has seq > maxSeq and is intentionally left in hot.
	if _, err := tx.Exec(ctx,
		`DELETE FROM log_entries WHERE job_id = $1 AND seq <= $2`, jobID, maxSeq); err != nil {
		return err
	}
	// Record the archive.
	if _, err := tx.Exec(ctx, `
		INSERT INTO log_archives (job_id, object_key, byte_size, entry_count)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (job_id) DO UPDATE SET
		  byte_size   = EXCLUDED.byte_size,
		  entry_count = EXCLUDED.entry_count,
		  object_key  = EXCLUDED.object_key,
		  compacted_at = now()`,
		jobID, key, bytes, count); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

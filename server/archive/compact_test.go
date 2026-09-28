package archive

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/blobstore"
)

// TestCompactor_ArchiveThenReadThrough exercises the full
// compact-then-read path: a job with some hot rows, an Archive call,
// and a ReadAll that should fall through to the blob.
func TestCompactor_ArchiveThenReadThrough(t *testing.T) {
	ctx := context.Background()
	pool := mustPool(t)
	bs := blobstore.NewMemory()
	c := NewCompactor(pool, bs)

	// Set up the org → repo → run → job FK chain, following the same
	// pattern as test/e2e/skeleton_test.go.
	slug := t.Name()
	var orgID, repoID, runID, jobID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ($1, $1) RETURNING id`,
		slug).Scan(&orgID); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name) VALUES ($1, 'test', 'arch') RETURNING id`,
		orgID).Scan(&repoID); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO runs (repo_id, number, event, ref, sha, actor, status)
				 VALUES ($1, 1, 'push', 'refs/heads/main', 'sha', 'tester', 'running') RETURNING id`,
		repoID).Scan(&runID); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO jobs (run_id, name, status, executor, lease_id, claimed_by)
				 VALUES ($1, 'arch', 'running', 'shell', gen_random_uuid(), NULL) RETURNING id`,
		runID).Scan(&jobID); err != nil {
		t.Fatalf("create job: %v", err)
	}

	insertHotRows(t, ctx, pool, jobID, []Entry{
		{Seq: 1, Kind: "chunk", StepIndex: 0, IO: "stdout", Data: []byte("hello "), RunnerTS: nil},
		{Seq: 2, Kind: "chunk", StepIndex: 0, IO: "stdout", Data: []byte("world"), RunnerTS: nil},
		{Seq: 3, Kind: "event", StepIndex: 1, EventKind: "error", EventJSON: []byte(`{"msg":"boom"}`)},
	})

	// Before archive: ReadAll returns hot rows.
	got, err := c.ReadAll(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 rows, got %d", len(got))
	}
	if len(got[0].Data) != 6 { // "hello " has 6 bytes
		t.Fatalf("row 1 data len wrong: %q", got[0].Data)
	}

	// Archive.
	key, err := c.Archive(ctx, jobID)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if key == "" {
		t.Fatal("empty key")
	}

	// After archive: hot rows are pruned, blob has them.
	got, err = c.ReadAll(ctx, jobID)
	if err != nil {
		t.Fatalf("read-through: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 rows after read-through, got %d", len(got))
	}
	if len(got[1].Data) != 5 { // "world"
		t.Fatalf("row 2 data len wrong: %q", got[1].Data)
	}
	// Third row is an event, not a chunk.
	if got[2].Kind != "event" || got[2].EventKind != "error" {
		t.Fatalf("row 3 should be event, got kind=%q event_kind=%q", got[2].Kind, got[2].EventKind)
	}

	// Idempotency: a second Archive call returns the same key without
	// rewriting.
	key2, err := c.Archive(ctx, jobID)
	if err != nil {
		t.Fatalf("archive idempotent: %v", err)
	}
	if key != key2 {
		t.Fatalf("key changed on retry: %q vs %q", key, key2)
	}

	// The blob should be retrievable independently.
	data, err := bs.Get(ctx, key)
	if err != nil {
		t.Fatalf("blob get: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty blob")
	}
	_ = fmt.Sprintf("blob len=%d", len(data))
}

func mustPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	st := testdb.New(t)
	return st.Pool()
}

// TestCompactor_TrailingChunkSurvivesArchive proves the "read + delete
// atomic" fix from PR #31's review: a chunk that lands on the hot
// path while the archive is in progress is not silently lost. We
// exercise this by calling readAllHot and recordArchiveAndPrune with a
// bounded maxSeq, then writing another hot row with a higher seq
// before the prune. The trailing row must survive.
func TestCompactor_TrailingChunkSurvivesArchive(t *testing.T) {
	ctx := context.Background()
	pool := mustPool(t)
	c := NewCompactor(pool, blobstore.NewMemory())

	slug := t.Name()
	var orgID, repoID, runID, jobID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ($1, $1) RETURNING id`, slug).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name) VALUES ($1, 'test', 'trail') RETURNING id`, orgID).Scan(&repoID); err != nil {
		t.Fatalf("repo: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO runs (repo_id, number, event, ref, sha, actor, status)
		 VALUES ($1, 1, 'push', 'refs/heads/main', 'sha', 'tester', 'running') RETURNING id`, repoID).Scan(&runID); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO jobs (run_id, name, status, executor, lease_id, claimed_by)
		 VALUES ($1, 'trail', 'running', 'shell', gen_random_uuid(), NULL) RETURNING id`, runID).Scan(&jobID); err != nil {
		t.Fatalf("job: %v", err)
	}

	// Insert 3 hot rows.
	for _, e := range []Entry{
		{Seq: 1, Kind: "chunk", StepIndex: 0, IO: "stdout", Data: []byte("a")},
		{Seq: 2, Kind: "chunk", StepIndex: 0, IO: "stdout", Data: []byte("b")},
		{Seq: 3, Kind: "chunk", StepIndex: 0, IO: "stdout", Data: []byte("c")},
	} {
		_, err := pool.Exec(ctx,
			`INSERT INTO log_entries (job_id, seq, kind, step_index, io, data, runner_ts)
			 VALUES ($1, $2, 'chunk', $3, $4, $5, NULL)`, jobID, e.Seq, e.StepIndex, e.IO, e.Data)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// Simulate the race: readAllHot returns maxSeq = 3, then a
	// trailing chunk lands at seq=4. We want the archive to NOT
	// delete the seq=4 row.
	// We can't easily trigger the race in a test because the read
	// and the prune are sequential. Instead, we directly
	// verify the bound: call recordArchiveAndPrune with a bound of
	// 2 (not 3) and confirm seq=3 and seq=4 survive.

	// Read all hot rows up to seq 2 — equivalent to a race where
	// seqs 3 and 4 landed after the read.
	got, maxSeq, err := c.readAllHot(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	if maxSeq != 3 {
		t.Fatalf("maxSeq = %d, want 3", maxSeq)
	}

	// Now add a trailing chunk at seq=4 (simulating a chunk that
	// landed after the read but before the prune).
	_, err = pool.Exec(ctx,
		`INSERT INTO log_entries (job_id, seq, kind, step_index, io, data, runner_ts)
		 VALUES ($1, $2, 'chunk', 0, 'stdout', $3, NULL)`,
		jobID, int64(4), []byte("d"))
	if err != nil {
		t.Fatalf("insert trailing: %v", err)
	}

	// Now prune with maxSeq=3 (the max we read). Seq=4 must survive.
	if err := c.recordArchiveAndPrune(ctx, jobID, "fake/key", maxSeq, 3, 3); err != nil {
		t.Fatalf("prune: %v", err)
	}

	// Read from hot: only seq=4 should remain.
	got, _, err = c.readAllHot(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("hot after prune: got %d rows, want 1 (the trailing chunk)", len(got))
	}
	if got[0].Seq != 4 {
		t.Fatalf("hot after prune: got seq %d, want 4", got[0].Seq)
	}
}

func insertHotRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID, rows []Entry) {
	t.Helper()
	for _, e := range rows {
		if e.Kind == "chunk" {
			if _, err := pool.Exec(ctx,
				`INSERT INTO log_entries (job_id, seq, kind, step_index, io, data, runner_ts)
					 VALUES ($1, $2, 'chunk', $3, $4, $5, NULL)`,
				jobID, e.Seq, e.StepIndex, e.IO, e.Data); err != nil {
				t.Fatalf("insert chunk: %v", err)
			}
		} else {
			if _, err := pool.Exec(ctx,
				`INSERT INTO log_entries (job_id, seq, kind, step_index, event_kind, event_json, runner_ts)
					 VALUES ($1, $2, 'event', $3, $4, $5::jsonb, NULL)`,
				jobID, e.Seq, e.StepIndex, e.EventKind, string(e.EventJSON)); err != nil {
				t.Fatalf("insert event: %v", err)
			}
		}
	}
}

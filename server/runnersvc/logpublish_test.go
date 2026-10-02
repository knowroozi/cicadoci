package runnersvc

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/redact"
)

// CIC-82: LogPublish feeds live SSE viewers, so it must get the masked bytes
// that were persisted, never the runner's raw output.
func TestLogPublishGetsMaskedData(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	ctx := context.Background()

	const secret = "sk-live-0123456789"
	masker := redact.NewRegisteredMasker()
	masker.Register(secret)

	type published struct {
		kind string
		seq  int64
		data []byte
	}
	var got []published
	cfg := DefaultConfig()
	cfg.Masker = masker
	cfg.LogPublish = func(_ uuid.UUID, kind string, seq int64, data []byte) {
		got = append(got, published{kind, seq, append([]byte(nil), data...)})
	}
	svc := New(pool, queue.NewPostgres(pool), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	// A running job leased to one runner.
	var orgID, repoID, runID, runnerID, jobID, leaseID uuid.UUID
	for _, step := range []struct {
		sql  string
		args []any
		dst  *uuid.UUID
	}{
		{`INSERT INTO orgs (slug, name) VALUES ('mask', 'Mask') RETURNING id`, nil, &orgID},
		{`INSERT INTO repos (org_id, owner, name) VALUES ($1, 'acme', 'mask') RETURNING id`, []any{&orgID}, &repoID},
		{`INSERT INTO runs (repo_id, number, event) VALUES ($1, 1, 'push') RETURNING id`, []any{&repoID}, &runID},
		{`INSERT INTO runners (org_id, name) VALUES ($1, 'r1') RETURNING id`, []any{&orgID}, &runnerID},
	} {
		args := make([]any, len(step.args))
		for i, a := range step.args {
			args[i] = *(a.(*uuid.UUID))
		}
		if err := pool.QueryRow(ctx, step.sql, args...).Scan(step.dst); err != nil {
			t.Fatalf("seed (%s): %v", step.sql, err)
		}
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO jobs (run_id, name, status, lease_id, lease_expires_at, claimed_by)
		VALUES ($1, 'only', 'running', gen_random_uuid(), now() + interval '1 hour', $2)
		RETURNING id, lease_id`, runID, runnerID).Scan(&jobID, &leaseID); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	lsCfg := DefaultLogStreamConfig()
	throttle := NewThrottler(1 << 20)
	ups := []*runnerv1.LogUp{
		{Msg: &runnerv1.LogUp_Chunk{Chunk: &runnerv1.LogChunk{
			JobId: jobID.String(), LeaseId: leaseID.String(), Seq: 1,
			Data: []byte("token=" + secret + "\n"),
		}}},
		{Msg: &runnerv1.LogUp_Event{Event: &runnerv1.StructuredEvent{
			JobId: jobID.String(), LeaseId: leaseID.String(), Seq: 2,
			Kind: "step_finished", Json: `{"note":"` + secret + `"}`,
		}}},
	}
	for _, up := range ups {
		if _, _, _, err := svc.ingestLogUp(ctx, runnerID, up, &lsCfg, throttle); err != nil {
			t.Fatalf("ingestLogUp: %v", err)
		}
	}

	if len(got) != 2 {
		t.Fatalf("LogPublish calls = %d, want 2 (chunk + event)", len(got))
	}
	for i, want := range []struct {
		kind string
		seq  int64
	}{{"chunk", 1}, {"event", 2}} {
		p := got[i]
		if p.kind != want.kind || p.seq != want.seq {
			t.Errorf("publish %d = %s/%d, want %s/%d", i, p.kind, p.seq, want.kind, want.seq)
		}
		if len(p.data) == 0 {
			t.Errorf("publish %d (%s): no data; live viewers would see empty entries", i, p.kind)
		}
		if bytes.Contains(p.data, []byte(secret)) {
			t.Errorf("SECURITY: publish %d (%s) carries the raw secret: %q", i, p.kind, p.data)
		}
	}
}

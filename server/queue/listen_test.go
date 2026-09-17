package queue_test

import (
	"context"
	"testing"
	"time"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/queue"
)

// The doorbell must ring at COMMIT, not at statement time. If it rang earlier,
// a dispatcher woken by it could query the queue before the job was visible and
// conclude there was nothing to do — a job that sits until the next sweep.
func TestNotifyFiresOnEnqueue(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runID := seedOrgRepoRun(t, st.Pool())
	notifications := make(chan queue.QueuedNotification, 8)

	listenerReady := make(chan struct{})
	go func() {
		close(listenerReady)
		_ = queue.Listen(ctx, st.DSN(), func(n queue.QueuedNotification) {
			select {
			case notifications <- n:
			default:
			}
		})
	}()
	<-listenerReady

	// Give the listener a moment to get its LISTEN in before we publish.
	// Missing a notification is not a correctness bug (the sweep covers it),
	// but it would make this test flaky about the thing it is checking.
	time.Sleep(500 * time.Millisecond)

	jobID := seedJob(t, st.Pool(), runID, withStatus("pending"), withTags("linux", "gpu"))
	if err := q.Enqueue(ctx, jobID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case n := <-notifications:
		if n.JobID != jobID.String() {
			t.Errorf("notified about job %s, want %s", n.JobID, jobID)
		}
		// Tags ride along so a dispatcher can ignore a doorbell for work no
		// runner it serves could take.
		if len(n.Tags) != 2 {
			t.Errorf("tags = %v, want the job's two required tags", n.Tags)
		}
		// The job must be visible to a claim by the time we hear about it.
		if s := jobStatus(t, st.Pool(), jobID); s != "queued" {
			t.Errorf("job status = %q when the doorbell rang; want queued", s)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no notification within 15s of enqueue")
	}
}

// A job inserted directly as 'queued' — the path the scheduler takes when a job
// has no `needs` edges — must ring the doorbell too, not just an UPDATE.
func TestNotifyFiresOnQueuedInsert(t *testing.T) {
	st := testdb.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runID := seedOrgRepoRun(t, st.Pool())
	notifications := make(chan queue.QueuedNotification, 8)

	go func() {
		_ = queue.Listen(ctx, st.DSN(), func(n queue.QueuedNotification) {
			select {
			case notifications <- n:
			default:
			}
		})
	}()
	time.Sleep(500 * time.Millisecond)

	jobID := seedJob(t, st.Pool(), runID) // inserted already queued

	select {
	case n := <-notifications:
		if n.JobID != jobID.String() {
			t.Errorf("notified about job %s, want %s", n.JobID, jobID)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no notification within 15s of a queued insert")
	}
}

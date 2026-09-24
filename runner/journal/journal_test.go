package journal

import "testing"

func TestRecordReplayTrim(t *testing.T) {
	j := NewMemoryJournal()

	j.Record(Entry{JobID: "job1", Lane: LaneStatus, Seq: 1, Msg: "s1"})
	j.Record(Entry{JobID: "job1", Lane: LaneStatus, Seq: 2, Msg: "s2"})
	j.Record(Entry{JobID: "job1", Lane: LaneLog, Seq: 1, Msg: "l1"})
	j.Record(Entry{JobID: "job1", Lane: LaneLog, Seq: 2, Msg: "l2"})
	j.Record(Entry{JobID: "job1", Lane: LaneLog, Seq: 3, Msg: "l3"})

	if got := j.Len(); got != 5 {
		t.Fatalf("Len = %d, want 5", got)
	}

	// Trim status up to seq 1: s1 gone, s2 remains.
	j.Trim("job1", LaneStatus, 1)
	if got := j.Replay("job1", LaneStatus, 0); len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("Replay status after Trim(1): %v", got)
	}

	// Trim log up to seq 2: l1,l2 gone, l3 remains.
	j.Trim("job1", LaneLog, 2)
	if got := j.Replay("job1", LaneLog, 0); len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("Replay log after Trim(2): %v", got)
	}

	// Replay with fromSeq=2 returns only seq=3.
	if got := j.Replay("job1", LaneLog, 2); len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("Replay log fromSeq=2: %v", got)
	}

	// Replay with fromSeq=3 returns nothing.
	if got := j.Replay("job1", LaneLog, 3); got != nil {
		t.Fatalf("Replay log fromSeq=3: %v, want nil", got)
	}

	// Trim to the point that job1 has no more entries.
	j.Trim("job1", LaneStatus, 2)
	j.Trim("job1", LaneLog, 3)
	if got := j.Len(); got != 0 {
		t.Fatalf("Len after full trim = %d, want 0", got)
	}
	if got := j.Jobs(); len(got) != 0 {
		t.Fatalf("Jobs after full trim = %v, want empty", got)
	}
}

func TestReplayEmptyJob(t *testing.T) {
	j := NewMemoryJournal()
	if got := j.Replay("nope", LaneStatus, 0); got != nil {
		t.Fatalf("Replay empty job: %v, want nil", got)
	}
	if got := j.Jobs(); len(got) != 0 {
		t.Fatalf("Jobs on empty journal: %v", got)
	}
	if got := j.Len(); got != 0 {
		t.Fatalf("Len on empty journal = %d, want 0", got)
	}
}

func TestMultipleJobs(t *testing.T) {
	j := NewMemoryJournal()
	j.Record(Entry{JobID: "a", Lane: LaneStatus, Seq: 1})
	j.Record(Entry{JobID: "b", Lane: LaneLog, Seq: 1})
	j.Record(Entry{JobID: "a", Lane: LaneStatus, Seq: 2})

	if got := j.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	jobs := j.Jobs()
	if len(jobs) != 2 {
		t.Fatalf("Jobs = %v, want 2 entries", jobs)
	}

	// Trim only job a.
	j.Trim("a", LaneStatus, 2)
	if got := j.Len(); got != 1 {
		t.Fatalf("Len after Trim = %d, want 1", got)
	}
	if jobs := j.Jobs(); len(jobs) != 1 || jobs[0] != "b" {
		t.Fatalf("Jobs after Trim = %v, want [b]", jobs)
	}
}

func TestConcurrentRecordTrim(t *testing.T) {
	j := NewMemoryJournal()
	done := make(chan struct{})
	done2 := make(chan struct{})

	go func() {
		for i := uint64(1); i <= 100; i++ {
			j.Record(Entry{JobID: "job", Lane: LaneStatus, Seq: i})
		}
		close(done)
	}()
	go func() {
		<-done
		// After all records, trim to 50 and verify.
		j.Trim("job", LaneStatus, 50)
		close(done2)
	}()
	<-done2
	if got := j.Len(); got != 50 {
		t.Fatalf("Len = %d, want 50", got)
	}
}

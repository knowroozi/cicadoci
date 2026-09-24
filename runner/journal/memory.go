package journal

import "sync"

// MemoryJournal is a process-memory Journal. It is the M1 production
// implementation: a runner-process restart loses the journal, the server's
// lease expires, and the job is requeued (at-most-once per lease).
//
// It is safe to use concurrently from the agent's send and receive loops
// without an external lock, because Record/Trim/Replay/Jobs/Len all take
// the internal mutex.
type MemoryJournal struct {
	mu sync.Mutex
	// byKey[jobID][lane] is an ascending-by-seq slice of entries.
	byKey map[string]map[Lane][]Entry
	// order is the global insertion order, used by Len and Jobs to
	// keep stable iteration without iterating the slice contents.
	order    []string
	orderIdx map[string]int
}

// NewMemoryJournal returns an empty, ready-to-use in-memory journal.
func NewMemoryJournal() *MemoryJournal {
	return &MemoryJournal{
		byKey:    make(map[string]map[Lane][]Entry),
		orderIdx: make(map[string]int),
	}
}

func (j *MemoryJournal) ensureJob(jobID string) {
	m, ok := j.byKey[jobID]
	if !ok {
		m = make(map[Lane][]Entry)
		j.byKey[jobID] = m
		j.orderIdx[jobID] = len(j.order)
		j.order = append(j.order, jobID)
	}
	_ = m
}

// Record appends an entry.
func (j *MemoryJournal) Record(e Entry) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ensureJob(e.JobID)
	lane := j.byKey[e.JobID][e.Lane]
	j.byKey[e.JobID][e.Lane] = append(lane, e)
}

// Trim discards entries <= upTo in the given job and lane.
func (j *MemoryJournal) Trim(jobID string, lane Lane, upTo uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	entries, ok := j.byKey[jobID][lane]
	if !ok || len(entries) == 0 {
		return
	}
	// Binary search for the last entry to keep.
	i := 0 // entries[i-1].Seq <= upTo, entries[i].Seq > upTo (if present)
	for i < len(entries) && entries[i].Seq <= upTo {
		i++
	}
	if i == 0 {
		return
	}
	j.byKey[jobID][lane] = entries[i:]
	if len(j.byKey[jobID][lane]) == 0 {
		// This lane is now empty; remove the whole lane map for this job if
		// all lanes are drained.
		j.cleanupJobLocked(jobID)
	}
}

// Replay returns all unacked entries for jobID/lane with seq > fromSeq.
func (j *MemoryJournal) Replay(jobID string, lane Lane, fromSeq uint64) []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	all, ok := j.byKey[jobID][lane]
	if !ok || len(all) == 0 {
		return nil
	}
	// Find the first entry with seq > fromSeq.
	i := 0
	for i < len(all) && all[i].Seq <= fromSeq {
		i++
	}
	if i == len(all) {
		return nil
	}
	out := make([]Entry, len(all)-i)
	copy(out, all[i:])
	return out
}

// Jobs returns job IDs that currently have at least one unacked entry.
func (j *MemoryJournal) Jobs() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []string
	for _, jobID := range j.order {
		if j.byKey[jobID] != nil {
			out = append(out, jobID)
		}
	}
	return out
}

// Len returns the total number of unacked entries.
func (j *MemoryJournal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := 0
	for _, lanes := range j.byKey {
		for _, entries := range lanes {
			n += len(entries)
		}
	}
	return n
}

// cleanupJobLocked removes all records for a job if every lane is empty. It
// must be called with mu held.
func (j *MemoryJournal) cleanupJobLocked(jobID string) {
	j.byKey[jobID] = nil
	delete(j.byKey, jobID)
	delete(j.orderIdx, jobID)
	// Remove from order in reverse to keep the index array coherent.
	for i := len(j.order) - 1; i >= 0; i-- {
		if j.order[i] == jobID {
			j.order = append(j.order[:i], j.order[i+1:]...)
			break
		}
	}
}

// Forget discards all recorded entries for jobID across every lane.  Used
// when a job has been ABANDONed and will never receive further acks, so its
// remaining journal entries would otherwise be a permanent memory leak.
func (j *MemoryJournal) Forget(jobID string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.byKey[jobID] == nil {
		return
	}
	j.cleanupJobLocked(jobID)
}

var _ Journal = (*MemoryJournal)(nil)

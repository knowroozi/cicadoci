// Package journal provides a bounded, per-job record of runner outbound
// messages that have not yet been acknowledged by the server.
//
// The protocol (project-docs/04-runner-protocol §6–§7) assigns one
// sequence-number space per job shared by log chunks and structured events,
// and a separate runner-wide counter for status updates.  The journal tracks
// both lanes per job so that on reconnect the runner can replay anything the
// server has not yet acknowledged, advancing from the server's high-water mark.
//
// The M1 implementation (MemoryJournal, in memory.go) keeps everything in
// process memory.  A runner-process restart loses the journal — the server's
// lease expires and the job is requeued, which is the documented
// at-most-once-per-lease behaviour.  A M3+ disk-backed journal with gap
// markers would make this survive a runner-process restart within the lease
// window; that is deferred.
package journal

// Lane identifies which stream a message belongs to.
type Lane int

const (
	// LaneStatus is the status-update lane, acked via StatusAck on the
	// Session (control) stream.
	LaneStatus Lane = iota
	// LaneLog is the log-chunk and structured-event lane, acked via LogAck
	// on the LogStream (bulk) stream.
	LaneLog
)

// Entry is one recorded unacked message.  Msg holds either a
// *runnerv1.StatusUpdate (LaneStatus) or a *runnerv1.LogUp (LaneLog); the
// lane field tells the caller which type to expect.
type Entry struct {
	JobID string
	Lane  Lane
	Seq   uint64
	Msg   any // *runnerv1.StatusUpdate or *runnerv1.LogUp
}

// Journal is the interface consumed by the runner agent.
type Journal interface {
	// Record appends an entry.  It is safe to call concurrently.
	Record(Entry)
	// Trim discards all recorded entries for jobID in lane with seq
	// <= upTo.  It is safe to call concurrently.
	Trim(jobID string, lane Lane, upTo uint64)
	// Replay returns all unacked entries for jobID in lane with seq
	// > fromSeq, ordered by ascending seq.
	Replay(jobID string, lane Lane, fromSeq uint64) []Entry
	// Jobs returns the job IDs that currently have at least one unacked
	// entry in any lane.  It is a snapshot; a concurrent Record for a
	// job that has just been trimmed may or may not appear.
	Jobs() []string
	// Len returns the total number of unacked entries across all lanes
	// and jobs.  Intended for diagnostics and tests.
	Len() int
}

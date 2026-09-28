// The SSE handler turns a job's log stream — a Compactor's historical
// entries plus a live Hub subscription — into a text/event-stream
// response.
//
// Resume protocol (per the ticket): the client sends a
// `Last-Event-ID` header carrying the seq of the last chunk it
// received. The handler:
//
//  1. Subscribes to the Hub first with `fromSeq = resumeSeq` so
//     no live entries written between the replay and the subscribe
//     are lost (the Hub has no history; the fix for that gap is to
//     subscribe before replay).
//  2. Reads the historical segment from the Compactor and writes
//     entries with `seq > resumeSeq`, tracking the highest seq
//     written so far.
//  3. Drains the live subscription, skipping any entry whose seq
//     is ≤ the highest seq already replayed from the Compactor
//     (deduplication at the replay/live boundary).
//
// This ordering eliminates the race where an entry written to
// Postgres between the Compactor read and the Hub subscribe
// would be invisible: the subscription is open before the read,
// so that entry is caught in the live channel and deduped
// against the replay set if it overlaps.
//
// Each event carries its seq in the `id:` field so the client
// can persist it and re-send on reconnect.
package logs

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/archive"
)

// SSE is the HTTP handler that serves the per-job log stream as
// Server-Sent Events.
//
// One SSE handler is shared across all job log endpoints.
type SSE struct {
	// hub delivers live entries. It must not be nil.
	hub *Hub
	// compactor is optional: when nil, no historical replay is
	// performed and the handler just live-tails.
	compactor *archive.Compactor
}

// NewSSE builds an SSE handler wired to a Hub and a Compactor.
//
// A nil Hub or Compactor is permitted: the handler degrades to
// "no live events" or "no replay" respectively.
func NewSSE(hub *Hub, comp *archive.Compactor) *SSE {
	if hub == nil {
		hub = NewHub(1024)
	}
	return &SSE{hub: hub, compactor: comp}
}

// ServeHTTP serves the SSE stream for a single job, identified by the
// `?job=` query parameter.
//
// The handler blocks for the request's lifetime: it does not return
// until the client disconnects. The stdlib ServeMux runs each request
// on its own goroutine, so serving is fine.
func (s *SSE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	jobID, err := uuid.Parse(r.URL.Query().Get("job"))
	if err != nil {
		http.Error(w, `{"code":"bad_request","message":"invalid job id"}`, http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"code":"internal_error","message":"streaming unsupported"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8") //nolint:staticcheck // set before body write
	w.Header().Set("Cache-Control", "no-cache")                        //nolint:staticcheck
	w.Header().Set("Connection", "keep-alive")                         //nolint:staticcheck
	//nolint:bodyclose // the response is the event stream; it stays open for the request.
	w.WriteHeader(http.StatusOK)

	resumeSeq := parseResumeSeq(r.Header.Get("Last-Event-ID"))

	// Subscribe first so any entry written between the replay read
	// and the subscribe call is not lost. The Hub has no history,
	// so opening the subscription before the read closes that gap.
	sub := s.hub.Subscribe(jobID, resumeSeq)
	defer s.hub.Unsubscribe(jobID, sub)

	// Replay the historical segment, tracking the highest seq we've
	// written so live dedup is correct.
	lastReplayed := resumeSeq
	if s.compactor != nil {
		entries, err := s.compactor.ReadAll(r.Context(), jobID)
		if err == nil {
			for _, e := range entries {
				if e.Seq <= resumeSeq {
					continue // already seen by the client
				}
				if err := writeEvent(w, flusher, e); err != nil {
					return
				}
				lastReplayed = e.Seq
			}
		}
		// On read error, skip replay and go live. The subscription
		// is open, so live events still flow.
	}

	// Drain the live subscription, deduplicating against the replay
	// window: any entry with seq ≤ lastReplayed was already written
	// from the Compactor read.
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.Done:
			// Hub dropped us for backpressure.
			return
		case e, ok := <-sub.Entries:
			if !ok {
				return
			}
			if e.Seq <= lastReplayed {
				continue // dedupe: already replayed
			}
			if err := writeEvent(w, flusher, e); err != nil {
				return
			}
		}
	}
}

// writeEvent writes a single SSE event:
//
//	id: <seq>
//	event: log | logevent
//	data: <raw bytes>
//	<blank line>
//
// The data field carries the raw bytes of the entry. A structured
// JSON envelope is deferred to a follow-up.
func writeEvent(w http.ResponseWriter, f http.Flusher, e archive.Entry) error {
	var b []byte
	b = append(b, "id: "...)
	b = append(b, []byte(strconv.FormatInt(e.Seq, 10))...)
	b = append(b, '\n')
	if e.Kind == "event" {
		b = append(b, "event: logevent\n"...)
	} else {
		b = append(b, "event: log\n"...)
	}
	b = append(b, "data: "...)
	b = append(b, e.Data...)
	b = append(b, '\n', '\n')
	if _, err := w.Write(b); err != nil {
		return err
	}
	f.Flush()
	return nil
}

// parseResumeSeq parses the Last-Event-ID header.
//
// A missing or malformed header returns 0, meaning "from the
// beginning".
func parseResumeSeq(v string) int64 {
	if v == "" {
		return 0
	}
	seq, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return seq
}

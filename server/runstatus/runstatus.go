// Package runstatus manages the "one message per run, edited in place"
// UX pattern (CIC-52) that contrasts with GitHub Actions' notification
// firehose.
//
// Each run gets a single top-level chat message that is updated in
// place as the run progresses:
//
//  1. Run starts → SendMessage posts the initial message with a
//     "running" accent.
//
//  2. Each step transitions → OpenThread posts a step-detail message
//     (below the parent, in-thread).
//
//  3. Run finishes → The top-level message is updated with a final
//
//     accent (green/red) and a link to the run.
package runstatus

import (
	"context"
	"fmt"

	"cicadoci.com/cicado/server/chat"
)

// Tracker updates a chat message in place as a run progresses.
//
// It implements the "no channel spam" UX: one parent message
// with step detail in the thread.
type Tracker struct {
	provider chat.ChatProvider
	channel  string
	msgID    chat.MessageID
	runID    string
}

// NewTracker creates a tracker that posts to channel via provider.
func NewTracker(ctx context.Context, provider chat.ChatProvider, channel, runID string) (*Tracker, error) {
	// Initial message: "Run running..."
	p, err := provider.SendMessage(ctx, channel, chat.MessagePayload{
		Text:  fmt.Sprintf("Run **%s** — running", runID),
		Color: "#dba622", // amber/running
	})
	if err != nil {
		return nil, fmt.Errorf("runstatus: initial message: %w", err)
	}
	return &Tracker{
		provider: provider,
		channel:  channel,
		msgID:    p,
		runID:    runID,
	}, nil
}

// StepUpdate is a single step progress event.
type StepUpdate struct {
	StepIndex int
	StepName  string
	State     string // queued, running, passed, failed, skipped
}

// OnStep emits a step-progress thread message.
func (t *Tracker) OnStep(ctx context.Context, s StepUpdate) error {
	text := fmt.Sprintf("Step %d %s — %s", s.StepIndex, s.StepName, s.State)
	_, err := t.provider.OpenThread(ctx, t.channel, string(t.msgID), chat.MessagePayload{
		Text: text,
	})
	return err
}

// OnFinish updates the top-level message with the final state.
//
// passed indicates whether the run passed. This replaces the "running"
// accent with green (#36a64f) or red (#e01e5a), and links to the run.
func (t *Tracker) OnFinish(ctx context.Context, passed bool) error {
	color, label := "#36a64f", "passed" // green
	if !passed {
		color, label = "#e01e5a", "failed" // red
	}
	payload := chat.MessagePayload{
		Text:  fmt.Sprintf("Run **%s** — %s", t.runID, label),
		Color: color,
		Steps: []chat.StepProgress{},
	}
	return t.provider.UpdateMessage(ctx, t.channel, string(t.msgID), payload)
}

// StepProgress is a per-step status for the progress list.
type StepProgress = chat.StepProgress

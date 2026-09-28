package chat

import (
	"context"
	"fmt"
	"sync"
)

// SlackProvider is an in-memory ChatProvider for testing.
//
// It satisfies the ChatProvider interface without making any HTTP
// calls. Real Slack integration (Socket Mode + Events API, CIC-49)
// replaces this in production.
type SlackProvider struct {
	mu       sync.Mutex
	messages []MessageRecord
}

// MessageRecord is a record of a sent/updated message, for
// assertions in tests.
type MessageRecord struct {
	Channel  string
	MsgID    MessageID
	Payload  MessagePayload
	ParentID string         // empty for top-level messages
	Action   Action         // terminal action; see Action constants below
	Status   ApprovalStatus // set by CloseApproval; ApprovalPending otherwise
}

// Action classifies the kind of message operation recorded.
type Action string

// Message record actions.
const (
	// ActionSend is set when a new message is posted.
	ActionSend Action = "send"
	// ActionUpdate is set when a message is edited in place.
	ActionUpdate Action = "update"
	// ActionThread is set when a reply is posted in a thread.
	ActionThread Action = "thread"
	// ActionApproval is set when approval buttons are rendered.
	ActionApproval Action = "approval"
	// ActionClose is set when an approval gate is resolved.
	ActionClose Action = "close"
)

// NewSlackProvider creates a new in-memory Slack provider.
func NewSlackProvider() *SlackProvider {
	return &SlackProvider{}
}

// SendMessage posts a new message.
func (s *SlackProvider) SendMessage(_ context.Context, channel string, payload MessagePayload) (MessageID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := MessageID(fmt.Sprintf("msg-%d", len(s.messages)+1))
	s.messages = append(s.messages, MessageRecord{
		Channel: channel,
		MsgID:   id,
		Payload: payload,
		Action:  ActionSend,
	})
	return id, nil
}

// UpdateMessage edits an existing message.
func (s *SlackProvider) UpdateMessage(_ context.Context, channel, msgID string, payload MessagePayload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.messages {
		if m.Channel == channel && string(m.MsgID) == msgID {
			s.messages[i].Payload = payload
			s.messages[i].Action = ActionUpdate
			return nil
		}
	}
	return fmt.Errorf("message %q not found in channel %q", msgID, channel)
}

// OpenThread posts the first message in a thread.
func (s *SlackProvider) OpenThread(_ context.Context, channel, parentMsgID string, payload MessagePayload) (MessageID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := MessageID(fmt.Sprintf("thread-%d", len(s.messages)+1))
	s.messages = append(s.messages, MessageRecord{
		Channel:  channel,
		MsgID:    id,
		Payload:  payload,
		ParentID: parentMsgID,
		Action:   ActionThread,
	})
	return id, nil
}

// RequestApproval renders approval buttons.
func (s *SlackProvider) RequestApproval(_ context.Context, channel, msgID string, req ApprovalRequest) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	triggerID := fmt.Sprintf("trigger-%d", len(s.messages)+1)
	s.messages = append(s.messages, MessageRecord{
		Channel: channel,
		MsgID:   MessageID(msgID),
		Payload: MessagePayload{
			Text: req.Reason,
			Buttons: []Button{
				{Text: "Approve", ActionID: req.Allow},
				{Text: "Deny", ActionID: req.Deny},
			},
		},
		Action: ActionApproval,
	})
	return triggerID, req.Allow, nil
}

// CloseApproval marks an approval as resolved with the given terminal
// status. The status is recorded on the MessageRecord so that tests
// can distinguish approved vs denied vs timed-out approvals.
func (s *SlackProvider) CloseApproval(_ context.Context, channel, msgID string, status ApprovalStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.messages {
		if m.Channel == channel && string(m.MsgID) == msgID {
			s.messages[i].Action = ActionClose
			s.messages[i].Status = status
			return nil
		}
	}
	return fmt.Errorf("approval %q not found", msgID)
}

// Records returns a copy of the message records.
func (s *SlackProvider) Records() []MessageRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MessageRecord, len(s.messages))
	copy(out, s.messages)
	return out
}

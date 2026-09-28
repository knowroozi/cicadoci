// Package chat defines the ChatProvider interface and the Slack driver.
//
// The interface is the seam between the approval/ChatOps subsystem and
// the specific chat platform. In MVP, Slack is the only driver, but
// Teams, Discord, and Mattermost are planned as later drivers.
//
// The security invariant that matters here: interactive buttons (e.g.
// approval gates) are clickable by ANY channel member, so the handler
// that processes a button click MUST authorize the acting user
// individually — never against a channel default-bot identity.
package chat

import "context"

// ChatProvider is the interface that the approval/ChatOps subsystem
// drives. Slack is the sole driver in MVP; Teams/Discord/Mattermost
// become later drivers.
//
// Implementations MUST:
//   - Acknowledge interactive payloads within 3 seconds (Slack's
//     contract) to avoid the "action took too long" error.
//   - Authorize the acting user for every interactive action.
type ChatProvider interface {
	// SendMessage posts a new message to a channel.
	SendMessage(ctx context.Context, channel string, payload MessagePayload) (MessageID, error)

	// UpdateMessage edits an existing message in place.
	//
	// Used for the "one message per run, edited in place" UX pattern
	// (CIC-52).
	UpdateMessage(ctx context.Context, channel, msgID string, payload MessagePayload) error

	// OpenThread posts the first message in a thread beneath msgID.
	// Threaded run status (CIC-52) posts step detail in the thread.
	OpenThread(ctx context.Context, channel, parentMsgID string, payload MessagePayload) (MessageID, error)

	// RequestApproval renders an interactive approval action (e.g.
	// Block Kit buttons in Slack). The handler that processes the
	// callback MUST authorize the acting user individually.
	//
	// The triggerID (e.g. Slack's trigger_id) is returned for use in
	// modal-opening calls that must happen before slow work.
	RequestApproval(ctx context.Context, channel, msgID string, request ApprovalRequest) (triggerID string, callbackID string, err error)

	// CloseApproval marks an approval as resolved (approved/denied/
	// timed-out) and clears the interactive buttons so they can no
	// longer be clicked.
	CloseApproval(ctx context.Context, channel, msgID string, status ApprovalStatus) error
}

// MessagePayload is a platform-agnostic message. The concrete driver
// marshals this into the platform's native format.
type MessagePayload struct {
	// Text is the primary body.
	Text string

	// Title is an optional bold header.
	Title string

	// Steps is an optional per-step progress list.
	Steps []StepProgress

	// Color is an optional accent color (hex, e.g. "#36a64f").
	Color string

	// Buttons is an optional list of interactive buttons.
	Buttons []Button
}

// StepProgress is a single step's status in a progress list.
type StepProgress struct {
	ID    string
	Name  string
	State string // queued, running, passed, failed, skipped
}

// Button is an interactive action rendered in a message.
type Button struct {
	// Text is the button label.
	Text string

	// ActionID is the callback identifier sent back on click.
	ActionID string

	// Style is an optional visual style (e.g. "primary", "danger").
	Style string

	// Confirm is an optional confirmation modal title.
	Confirm string
}

// ApprovalRequest is a request to render an approval gate.
type ApprovalRequest struct {
	// RunID is the run being gated.
	RunID string

	// Reason is a human-readable reason shown to the approver.
	Reason string

	// Allow is the action ID for "approve".
	Allow string

	// Deny is the action ID for "deny".
	Deny string

	// TimeoutSec is the approval timeout in seconds.
	TimeoutSec int

	// AllowedUsers is the allow-list of user IDs who can approve.
	//
	// This is the security gate: a button click is authorized against
	// this list, not against a channel default.
	AllowedUsers []string
}

// ApprovalStatus is the terminal status of an approval.
type ApprovalStatus int

// Approval terminal statuses.
const (
	// ApprovalPending is the initial state.
	ApprovalPending ApprovalStatus = 0

	// ApprovalApproved means a user in the allow-list approved.
	ApprovalApproved ApprovalStatus = 1

	// ApprovalDenied means a user in the allow-list denied.
	ApprovalDenied ApprovalStatus = 2

	// ApprovalTimedOut means no one acted before the deadline.
	ApprovalTimedOut ApprovalStatus = 3
)

// MessageID is an opaque message identifier.
type MessageID string

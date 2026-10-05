// Package chat: no-op ChatProvider for local development.
//
// A NoOpProvider satisfies ChatProvider without contacting any external
// service. It is used by cicado-server when approval gates are enabled
// but no Slack app is configured.
package chat

import "context"

// NoOpProvider is a ChatProvider that does nothing. It is for
// development and testing: approval gates can be opened, state tracked,
// and resolved, but no messages are actually posted to any chat platform.
//
// The security model is identical to any real provider: the handler that
// calls Resolve STILL authorizes the acting user against the gate's
// allow-list. The no-op is only for the UI surface.
type NoOpProvider struct{}

// NewNoOpProvider returns a NoOpProvider.
func NewNoOpProvider() *NoOpProvider {
	return &NoOpProvider{}
}

func (n *NoOpProvider) SendMessage(
	_ context.Context,
	_ string,
	_ MessagePayload,
) (MessageID, error) {
	return "noop", nil
}

func (n *NoOpProvider) UpdateMessage(
	_ context.Context,
	_, _ string,
	_ MessagePayload,
) error {
	return nil
}

func (n *NoOpProvider) OpenThread(
	_ context.Context,
	_, _ string,
	_ MessagePayload,
) (MessageID, error) {
	return "noop", nil
}

func (n *NoOpProvider) RequestApproval(
	_ context.Context,
	_, _ string,
	_ ApprovalRequest,
) (string, string, error) {
	return "trigger-noop", "callback-noop", nil
}

func (n *NoOpProvider) CloseApproval(
	_ context.Context,
	_, _ string,
	_ ApprovalStatus,
) error {
	return nil
}

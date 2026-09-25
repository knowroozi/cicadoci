package webhook

import (
	"encoding/json"
	"errors"
)

// ── event types ───────────────────────────────────────────────────

// These are the GitHub webhook event names CICaDo consumes.  The event name
// comes from the X-GitHub-Event header.
const (
	EventPush        = "push"
	EventPullRequest = "pull_request"
	EventCheckSuite  = "check_suite"
	EventCheckRun    = "check_run"
)

// ── payload structures ────────────────────────────────────────────

// PushPayload is the minimal subset of GitHub's push event that we care about.
// Unknown fields are ignored by json.Unmarshal.
type PushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"` // commit SHA
	Before     string `json:"before"`
	Repository struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
	} `json:"repository"`
	HeadCommit struct {
		ID string `json:"id"`
	} `json:"head_commit"`
	Pusher struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"pusher"`
}

// PullRequestPayload is the minimal subset of GitHub's pull_request event.
type PullRequestPayload struct {
	Action string `json:"action"` // "opened", "synchronize", "closed", etc.
	Number int    `json:"number"`
	Base   struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Fork       bool `json:"fork"`
	Repository struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
	} `json:"repository"`
}

// CheckSuitePayload is the minimal subset of GitHub's check_suite event.
type CheckSuitePayload struct {
	Action     string `json:"action"` // "requested"
	CheckSuite struct {
		ID         int64  `json:"id"`
		HeadSHA    string `json:"head_sha"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	} `json:"check_suite"`
}

// CheckRunPayload is the minimal subset of GitHub's check_run event
// (including the "rerequested" action).
type CheckRunPayload struct {
	Action   string `json:"action"` // "rerequested"
	CheckRun struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		HeadSHA    string `json:"head_sha"`
		StartedAt  string `json:"started_at"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	} `json:"check_run"`
}

// ── dispatch ───────────────────────────────────────────────────────

// Dispatcher receives decoded webhook events after signature and idempotency
// checks.  Each method handles one event type; unimplemented methods default
// to ErrUnimplemented which the handler maps to a 501.
type Dispatcher interface {
	HandlePush(payload PushPayload) error
	HandlePullRequest(payload PullRequestPayload) error
	HandleCheckSuite(payload CheckSuitePayload) error
	HandleCheckRun(payload CheckRunPayload) error
}

// ErrUnimplemented is returned by Dispatcher methods that have no
// implementation yet.  The handler maps it to 501 Not Implemented, which
// tells GitHub to retry on the next scheduled attempt rather than
// suppressing retries indefinitely.
var ErrUnimplemented = errors.New("webhook: event type not yet implemented")

// NoOpDispatcher is a Dispatcher that logs and does nothing.  It is useful
// for smoke-testing the signature/idempotency plumbing without wiring
// run-creation into the store.
type NoOpDispatcher struct{}

// HandlePush implements Dispatcher.
func (NoOpDispatcher) HandlePush(_ PushPayload) error { return nil }

// HandlePullRequest implements Dispatcher.
func (NoOpDispatcher) HandlePullRequest(_ PullRequestPayload) error { return nil }

// HandleCheckSuite implements Dispatcher.
func (NoOpDispatcher) HandleCheckSuite(_ CheckSuitePayload) error { return nil }

// HandleCheckRun implements Dispatcher.
func (NoOpDispatcher) HandleCheckRun(_ CheckRunPayload) error { return nil }

// ── routing ─────────────────────────────────────────────────────────

// Dispatch decodes a raw JSON event body and routes it to the matching
// Dispatcher method based on the event type.  Returns ErrUnknownEventType for
// events CICaDo doesn't handle, and the underlying decode/dispatch error
// otherwise.
func Dispatch(eventType string, raw []byte, d Dispatcher) error {
	switch eventType {
	case EventPush:
		var p PushPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		return d.HandlePush(p)

	case EventPullRequest:
		var p PullRequestPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		return d.HandlePullRequest(p)

	case EventCheckSuite:
		var p CheckSuitePayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		return d.HandleCheckSuite(p)

	case EventCheckRun:
		var p CheckRunPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		return d.HandleCheckRun(p)

	default:
		return errUnknownEventType
	}
}

// errUnknownEventType is returned when the event type is not recognised.
var errUnknownEventType = errors.New("webhook: unknown event type")

// IsUnknownEventType reports whether err is the "unknown event type" error.
// It exists so callers can do errors.Is(err, ...) without importing the
// unexported sentinel directly.
func IsUnknownEventType(err error) bool { return errors.Is(err, errUnknownEventType) }

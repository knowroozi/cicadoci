package webhook

import (
	"encoding/json"
	"errors"
)

// ── event types ───────────────────────────────────────────────────

// These are the GitHub webhook event names CICaDo consumes.  The event name
// comes from the X-GitHub-Event header.
const (
	EventPing              = "ping"
	EventPush              = "push"
	EventPullRequest       = "pull_request"
	EventCheckSuite        = "check_suite"
	EventCheckRun          = "check_run"
	EventInstallation      = "installation"
	EventInstallationRepos = "installation_repositories"
)

// ── payload structures ────────────────────────────────────────────

// PushPayload is the minimal subset of GitHub's push event that we care about.
// Unknown fields are ignored by json.Unmarshal.
type PushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"` // commit SHA
	Before     string `json:"before"`
	Repository struct {
		Name          string `json:"name"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
	} `json:"repository"`
	HeadCommit struct {
		ID string `json:"id"`
	} `json:"head_commit"`
	Pusher struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"pusher"`
}

//  pull_request 
//
// CIC-102: GitHub nests fork/base/head under "pull_request", not at the
// top level. The old flat struct caused Fork to be false and ref/SHA to
// be empty on real events, bypassing the fork-PR policy and GetJobSecrets
// guard. The fields below are the minimal subset CICaDo consumes.

// Repo is the minimal subset of a GitHub repository sub-object.
type Repo struct {
	FullName string `json:"full_name"`
	Fork     bool   `json:"fork"`
	// CloneURL is the repository's HTTPS clone URL (CIC-104). For the
	// base repository it is the checkout source for refs/pull/<N>/head.
	CloneURL string `json:"clone_url"`
}

// PullRequestHead represents GitHub's pull_request.head sub-object.
//
// Head.Repo is optional: GitHub may send null for deleted forks. When
// JSON null is received, Head.Repo is nil, which IsFork treats as fork.
type PullRequestHead struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *Repo  `json:"repo"`
}

// PullRequest is GitHub's nested pull_request object.
type PullRequest struct {
	Number int `json:"number"`
	Base   struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head PullRequestHead `json:"head"`
	Fork bool            `json:"fork"`
}

// PullRequestPayload is the top-level pull_request webhook payload.
// Action, PullRequest, and Repository are at the top level; Fork,
// Base, Head are nested under PullRequest (CIC-102 fix).
type PullRequestPayload struct {
	Action      string      `json:"action"`
	PullRequest PullRequest `json:"pull_request"`
	Repository  Repo        `json:"repository"`
	// Sender is the GitHub actor for this event.  GitHub places this at
	// the top level as "sender". Used as the audit actor for PR runs.
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// IsFork returns true when the pull request should be treated as a fork PR.
//
// Fork detection uses a single authoritative signal:
//
//	head.repo.full_name != repository.full_name  (different repo = fork).
//
// We do NOT use pull_request.fork or head.repo.fork because when the
// base repository is itself a GitHub fork, those fields report true
// even for same-repo PRs.
//
// Fail-closed conditions (any one is sufficient):
//   - head.repo is nil        (deleted fork or malformed payload)
//   - repository.full_name is empty      (cannot confirm same repo)
//   - head.repo.full_name is empty      (incomplete payload)
func (p PullRequestPayload) IsFork() bool {
	head := p.PullRequest.Head
	if head.Repo == nil {
		return true
	}
	if p.Repository.FullName == "" || head.Repo.FullName == "" {
		return true
	}
	return head.Repo.FullName != p.Repository.FullName
}

type CheckSuitePayload struct {
	Action     string `json:"action"` // "requested", "rerequested"
	CheckSuite struct {
		ID         int64  `json:"id"`
		HeadSHA    string `json:"head_sha"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	} `json:"check_suite"`
	// Repository is the top-level "repository" object, which is where
	// GitHub puts the repo on check_suite deliveries (CIC-81).
	Repository Repo `json:"repository"`
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
	// Repository is the top-level "repository" object, which is where
	// GitHub puts the repo on check_run deliveries (CIC-81).
	Repository Repo `json:"repository"`
}

// ── installation events (CIC-146) ────────────────────────────────────────

// InstallationPayload is GitHub's "installation" webhook event.
//
// action: "created", "deleted", "updated", "suspended", "unsuspended".
// When a GitHub App is installed or its access changes, GitHub delivers
// this event. The handler stores/updates repos.installation_id so that
// the clone minter can resolve the right installation.
type InstallationPayload struct {
	Action       string `json:"action"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"` // "User" or "Organization"
		} `json:"account"`
	} `json:"installation"`
	// Repositories are TOP-LEVEL in the GitHub 'installation' event (CIC-146 B3).
	// Each entry has 'name' (not 'full_name'); the handler uses that.
	Repositories []GitRepo `json:"repositories"`
}

// InstallationRepositoriesPayload is GitHub's "installation_repositories"
// webhook event, delivered when repositories are added to or removed
// from an installation.
//
// action: "added_target", "removed_target", "added", "removed",
// "added", "removed". The added/removed lists contain minimal repo
// metadata; only full_name is consumed by the handler.
type InstallationRepositoriesPayload struct {
	Action       string `json:"action"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	} `json:"installation"`
	RepositoriesAdded   []GitRepo `json:"repositories_added"` // CIC-146: new
	RepositoriesRemoved []GitRepo `json:"repositories_removed"`
}

// GitRepo is the minimal GitHub repository representation used by
// installation_repositories events.
type GitRepo struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
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
	// HandleInstallation stores/updates installation_id for the org
	// from a GitHub App installation event.  Added in CIC-146.
	HandleInstallation(payload InstallationPayload) error
	// HandleInstallationRepos populates installation_id for repos added
	// to or removed from an installation.  Added in CIC-146.
	HandleInstallationRepos(payload InstallationRepositoriesPayload) error
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

// HandleInstallation implements Dispatcher (no-op; CIC-146).
func (NoOpDispatcher) HandleInstallation(_ InstallationPayload) error { return nil }

// HandleInstallationRepos implements Dispatcher (no-op; CIC-146).
func (NoOpDispatcher) HandleInstallationRepos(_ InstallationRepositoriesPayload) error { return nil }

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

	case EventPing:
		// ping is a GitHub webhook health-check; return successfully.
		return nil

	case EventInstallation:
		var p InstallationPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		return d.HandleInstallation(p)

	case EventInstallationRepos:
		var p InstallationRepositoriesPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		return d.HandleInstallationRepos(p)

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

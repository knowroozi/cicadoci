package runner

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
)

// fakeSecretsClient implements the narrow secretsClient surface for offline
// tests.
type fakeSecretsClient struct {
	secrets  map[string]string
	err      error
	jjobID   string
	jleaseID string
	calls    int
}

func (f *fakeSecretsClient) GetJobSecrets(_ context.Context, in *runnerv1.GetJobSecretsRequest,
	_ ...grpc.CallOption) (*runnerv1.GetJobSecretsResponse, error) {
	f.calls++
	f.jjobID = in.GetJobId()
	f.jleaseID = in.GetLeaseId()
	if f.err != nil {
		return nil, f.err
	}
	return &runnerv1.GetJobSecretsResponse{Secrets: f.secrets}, nil
}

// TestFetchJobSecrets_DeliversValues verifies the runner fetches secret values
// from the lease-scoped RPC and returns them normalized to a non-nil map.
func TestFetchJobSecrets_DeliversValues(t *testing.T) {
	fake := &fakeSecretsClient{
		secrets: map[string]string{
			"DEPLOY_ROLE":    "arn:aws:iam::123:role/deploy",
			"PROD_API_TOKEN": "abc",
		},
	}
	got, err := fetchJobSecrets(context.Background(), fake, "job-1", "lease-7", []string{"DEPLOY_ROLE", "PROD_API_TOKEN"})
	if err != nil {
		t.Fatalf("fetchJobSecrets: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d secrets, want 2", len(got))
	}
	if got["DEPLOY_ROLE"] != "arn:aws:iam::123:role/deploy" {
		t.Errorf("DEPLOY_ROLE = %q, want the arn", got["DEPLOY_ROLE"])
	}
	if fake.jjobID != "job-1" || fake.jleaseID != "lease-7" {
		t.Errorf("RPC called with job=%q lease=%q, want job-1 lease-7", fake.jjobID, fake.jleaseID)
	}
}

// TestFetchJobSecrets_EmptyIsNonNil confirms a job that references no secrets
// gets an empty but non-nil map, so downstream Resolve sees a defined value.
func TestFetchJobSecrets_EmptyIsNonNil(t *testing.T) {
	fake := &fakeSecretsClient{secrets: nil}
	got, err := fetchJobSecrets(context.Background(), fake, "j", "l", []string{"A"})
	if err != nil {
		t.Fatalf("fetchJobSecrets: %v", err)
	}
	if got == nil {
		t.Error("got nil map, want non-nil empty map")
	}
	if len(got) != 0 {
		t.Errorf("got %d secrets, want 0", len(got))
	}
}

// TestFetchJobSecrets_NoRefsSkipsRPC: a job whose assignment lists no
// secret_refs must not call GetJobSecrets at all, so a job without secrets
// never depends on the secrets path (CIC-133).
func TestFetchJobSecrets_NoRefsSkipsRPC(t *testing.T) {
	fake := &fakeSecretsClient{err: status.Error(codes.Unauthenticated, "must not be called")}
	got, err := fetchJobSecrets(context.Background(), fake, "j", "l", nil)
	if err != nil {
		t.Fatalf("fetchJobSecrets: %v", err)
	}
	if fake.calls != 0 {
		t.Errorf("GetJobSecrets called %d times, want 0", fake.calls)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("got %v, want non-nil empty map", got)
	}
}

// TestFetchJobSecrets_InfraError wraps a transport error with a clean prefix.
func TestFetchJobSecrets_InfraError(t *testing.T) {
	under := status.Error(codes.Unavailable, "upstream down")
	fake := &fakeSecretsClient{err: under}
	_, err := fetchJobSecrets(context.Background(), fake, "j", "l", []string{"A"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, under) {
		t.Errorf("error chain = %v, want to wrap %v", err, under)
	}
}

// TestFetchJobSecrets_NilClient treats a nil client as "no secrets": an empty
// map and no error, so a connection-less agent still runs its steps.
// Production agents always wire a live client, so this path is a test /
// pre-connect convenience, not a security posture.
func TestFetchJobSecrets_NilClient(t *testing.T) {
	got, err := fetchJobSecrets(context.Background(), nil, "j", "l", []string{"A"})
	if err != nil {
		t.Fatalf("nil client returned error: %v", err)
	}
	if got == nil {
		t.Error("expected a non-nil empty map for nil client")
	}
	if len(got) != 0 {
		t.Errorf("got %d secrets, want 0", len(got))
	}
}

// TestUnresolvedSecrets_ReturnsMissingNames is the runtime should-fix from
// Dwight's #91 review item 3: after resolve, if an env still contains a
// secret reference (i.e. the name was declared but not in the fetched map),
// unresolvedSecrets returns those names so the runner can fail the job with
// a clear "secret X not available" message.
func TestUnresolvedSecrets_ReturnsMissingNames(t *testing.T) {
	// A bare-form ref that was not resolved (secrets map missing the entry).
	env := map[string]string{
		"DEPLOY": "secrets.DEPLOY_ROLE", // unresolved
		"PLAIN":  "not-a-secret",        // not a ref
		"EMPTY":  "",                    // empty
	}
	missing := unresolvedSecrets(env)
	if len(missing) != 1 {
		t.Fatalf("expected 1 unresolved ref, got %v", missing)
	}
	if missing[0] != "DEPLOY_ROLE" {
		t.Errorf("unresolved = %v, want [DEPLOY_ROLE]", missing)
	}
}

// TestUnresolvedSecrets_Empty returns nil when there are no refs at all.
func TestUnresolvedSecrets_Empty(t *testing.T) {
	env := map[string]string{"FOO": "bar"}
	got := unresolvedSecrets(env)
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// TestUnresolvedSecrets_AllResolved is nil when every ref was resolved.
func TestUnresolvedSecrets_AllResolved(t *testing.T) {
	env := map[string]string{
		"DEPLOY": "arn:aws:iam::1:role/deploy",
		"API":    "tok_abc",
	}
	got := unresolvedSecrets(env)
	if len(got) != 0 {
		t.Errorf("got %v, want empty (all resolved)", got)
	}
}

// runner — secret delivery.
//
// secrets.go wires the lease-scoped GetJobSecrets RPC into job execution
// (CIC-79). The runner fetches SECRET VALUES for a job by job_id + lease_id;
// the server authenticates against the LIVE lease and returns only the
// references the job's secret_refs authorizes. Values are held in memory for
// the life of the executor and are never written to disk or the log stream
// (the server's redaction masker registers the values so incoming log chunks
// are masked).
package runner

import (
	"context"
	"fmt"
	"sort"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"cicadoci.com/cicado/internal/secretexpr"
	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
)

// secretsClient is the narrow surface the runner needs from the gRPC client to
// deliver secrets. The generated RunnerServiceClient satisfies it; extracting
// the interface lets the fetch path be unit-tested offline without a live server.
type secretsClient interface {
	GetJobSecrets(ctx context.Context, in *runnerv1.GetJobSecretsRequest, opts ...grpc.CallOption) (*runnerv1.GetJobSecretsResponse, error)
}

// fetchJobSecrets performs the lease-scoped GetJobSecrets call and returns the
// name->value map. A job that references no secrets legitimately gets an empty
// (non-nil) map. Transport / authorization failures are wrapped so the caller
// can classify them as infrastructure failures.
//
// refs is the job's secret_refs from the assignment. When it is empty the
// server would grant nothing, so the RPC is skipped: a job without secrets
// must not depend on the secrets path being reachable (CIC-133).
func fetchJobSecrets(ctx context.Context, client secretsClient, jobID, leaseID string, refs []string) (map[string]string, error) {
	// A nil client means the agent was constructed without a live gRPC
	// connection (unit tests, or a runner that has not connected yet). That is
	// NOT an error: the job simply carries no secrets and the step runs with an
	// empty secret set. Only a live RPC failure is an infrastructure error.
	if client == nil || len(refs) == 0 {
		return map[string]string{}, nil
	}
	resp, err := client.GetJobSecrets(ctx, &runnerv1.GetJobSecretsRequest{
		JobId:   jobID,
		LeaseId: leaseID,
	})
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.Unavailable {
			return nil, fmt.Errorf("runner: secrets unavailable: %w", err)
		}
		return nil, fmt.Errorf("runner: get job secrets: %w", err)
	}
	// Normalize a nil/empty response to a non-nil empty map so the downstream
	// resolver sees a defined value; a job with no refs is legitimate.
	out := make(map[string]string, len(resp.GetSecrets()))
	for k, v := range resp.GetSecrets() {
		out[k] = v
	}
	return out, nil
}

// unresolvedSecrets returns the distinct, sorted list of secret NAMES that are
// still present in env after resolution — i.e. references that were declared in
// the workflow but had no corresponding value in the fetched map. This is the
// "secret X not available" check Dwight's #91 should-fix item asks for: it
// surfaces a missing secret as a clear error at runtime rather than letting a
// step fail on an opaque auth error downstream.
//
// The returned list contains only NAMES, not values.
func unresolvedSecrets(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	for _, v := range env {
		if name, ok := secretexpr.Extract(v); ok {
			seen[name] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

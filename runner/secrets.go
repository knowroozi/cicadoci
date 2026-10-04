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
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"cicadoci.com/cicado/internal/secretexpr"

	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
)

// githubCloneTokenSecretName is the canonical secrets-store key for the
// per-repo GitHub token used to clone private repositories (CIC-148).
//
// This name is treated as a reserved *optional* secret: if it is absent
// from the secrets map, the runner sets CICADO_GITHUB_TOKEN to "" and
// the step does NOT fail as secret_unavailable.  This prevents every
// non-fork run against a public repository from failing at checkout when
// the operator has not provisioned a token.
const githubCloneTokenSecretName = "github_clone_token"

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

// unresolvedSecrets returns the distinct, sorted list of secret NAMES
// referenced by the pre-resolution env that have no corresponding value in
// secrets — i.e. references that were declared in the workflow but were not
// delivered by the fetch. This is the "secret X not available" check Dwight's
// #91 should-fix item asks for: it surfaces a missing secret as a clear error
// at runtime rather than letting a step fail on an opaque auth error
// downstream.
//
// It runs on the RAW, pre-resolution env and subtracts the names present in
// secrets, so it never re-parses a delivered secret's VALUE: a secret whose
// value itself contains a `secrets.` template or bare reference is delivered
// and must not be flagged, and must never have part of its content echoed back
// into the failure message or a log line (which do not go through the log
// masker) — that would leak secret content. The returned list contains only
// NAMES, not values.
func unresolvedSecrets(raw, secrets map[string]string) []string {
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	for _, v := range raw {
		// NamesIn reports every reference in a value, including embedded
		// ${{ secrets.NAME }} occurrences (CIC-135) and the bare whole-value
		// form. A name that is present in secrets was delivered and is
		// dropped.
		//
		// CIC-148: github_clone_token is a reserved *optional* secret.
		// When absent it is NOT flagged as unresolved: the runner sets
		// CICADO_GITHUB_TOKEN to "" so the clone falls back to anonymous
		// access (public repos, or unconfigured private repos fail naturally).
		for _, name := range secretexpr.NamesIn(v) {
			if _, delivered := secrets[name]; !delivered {
				if name == githubCloneTokenSecretName {
					continue
				}
				seen[name] = struct{}{}
			}
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

// resolveOptionalSecrets normalises the reserved optional secret
// github_clone_token in stepEnv.  See CIC-148 B2 and B4.
//
// stepRaw is the PRE-resolution env map (templates as written by the
// controller); stepEnv is the POST-resolution map (values delivered by
// GetJobSecrets).  The function blanks CICADO_GITHUB_TOKEN to "" ONLY
// when the raw value references github_clone_token AND the token is not
// present (absent or whitespace-only) in secrets.  A user's own
// CICADO_GITHUB_TOKEN that references a different secret name or is a
// literal must survive unchanged (B4).
//
// Returns a new map; neither stepEnv nor stepRaw is mutated.
func resolveOptionalSecrets(stepEnv, stepRaw map[string]string, secrets map[string]string) map[string]string {
	_, hasKey := stepEnv["CICADO_GITHUB_TOKEN"]
	if !hasKey {
		return stepEnv // no CICADO_GITHUB_TOKEN key — leave everything alone
	}
	rawVal, ok := stepRaw["CICADO_GITHUB_TOKEN"]
	if !ok {
		return stepEnv // no raw template — resolved from job-level env, leave as-is
	}
	// B4: only blank when the raw value actually references the reserved
	// github_clone_token name.  A user's own CICADO_GITHUB_TOKEN that
	// references a different secret (e.g. my_pat) or a literal string
	// must survive.
	hasCloneRef := false
	for _, n := range secretexpr.NamesIn(rawVal) {
		if n == githubCloneTokenSecretName {
			hasCloneRef = true
			break
		}
	}
	if !hasCloneRef {
		return stepEnv
	}
	// The raw value DOES reference github_clone_token.  Check presence.
	val, present := secrets[githubCloneTokenSecretName]
	if present && strings.TrimSpace(val) != "" {
		return stepEnv // real token delivered — pass through
	}
	out := make(map[string]string, len(stepEnv)+1)
	for k, v := range stepEnv {
		out[k] = v
	}
	out["CICADO_GITHUB_TOKEN"] = ""
	return out
}

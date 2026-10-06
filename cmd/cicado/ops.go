package main

// ops.go implements CIC-194's operator CLI subcommands -- thin HTTP clients
// over the server's operator write surface (secrets PUT/DELETE, job cancel).
// Before this the platform had no way to set/delete a secret or cancel a job;
// these subcommands close that gap. They are the exact counterpart of the
// server routes in server/api/writes.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// httpClient is the shared HTTP client every ops subcommand uses, with a
// bounded timeout so a hung server cannot hang the CLI indefinitely.
func httpClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// doRequest issues a single request against a CICaDo server, attaching the
// operator bearer token when non-empty, and returns the response status + body
// so a caller can shape its own output.
func doRequest(ctx context.Context, method, url, token string, body []byte) (int, []byte, error) {
	var r *http.Request
	var err error
	if len(body) > 0 {
		r, err = http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	} else {
		r, err = http.NewRequestWithContext(ctx, method, url, nil)
	}
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient().Do(r)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, nil
}

// runCtx is the context every ops request runs under. The CLI is short-lived,
// so Background suffices; the client's own 30s cap bounds a hung server.
var runCtx = context.Background()

// cmdSecrets dispatches `cicado secrets <set|delete> ...`.
func cmdSecrets(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("secrets: missing action; usage: cicado secrets <set|delete> [flags]")
	}
	switch args[0] {
	case "set":
		return cmdSecretSet(args[1:])
	case "delete":
		return cmdSecretDelete(args[1:])
	default:
		return fmt.Errorf("secrets: unknown action %q (try 'set' or 'delete')", args[0])
	}
}

// cmdSecretSet implements `cicado secrets set --org O --repo R --name N
// --value V` (or --value-stdin, so a long or sensitive value need not sit on
// the process-argument list).
func cmdSecretSet(args []string) error {
	fs := flag.NewFlagSet("secrets set", flag.ContinueOnError)
	var (
		server  string
		token   string
		org     string
		repo    string
		name    string
		value   string
		byStdin bool
	)
	fs.StringVar(&server, "server", envOr("CICADO_SERVER", ""), "server base URL, e.g. http://localhost:7800")
	fs.StringVar(&token, "token", envOr("CICADO_API_TOKEN", ""), "operator bearer token (or CICADO_API_TOKEN)")
	fs.StringVar(&org, "org", "", "org id (required)")
	fs.StringVar(&repo, "repo", "", "repo id (org-scoped when empty)")
	fs.StringVar(&name, "name", "", "secret name (required)")
	fs.StringVar(&value, "value", "", "secret value (or --value-stdin)")
	fs.BoolVar(&byStdin, "value-stdin", false, "read the value from stdin instead of --value")
	if err := fs.Parse(args); err != nil {
		return err
	}
	val := value
	if byStdin {
		// J1/non-blocking: surface a real read error instead of dropping it, and
		// strip a single trailing newline (the common `echo secret | cicado
		// ... --value-stdin` footgun); multi-line values are preserved otherwise.
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("secrets set: reading --value-stdin: %w", err)
		}
		val = strings.TrimSuffix(string(b), "\n")
	}
	if server == "" {
		return fmt.Errorf("secrets set: --server is required (or set CICADO_SERVER)")
	}
	if org == "" || name == "" || val == "" {
		return fmt.Errorf("secrets set: --org, --name, and a value (--value or --value-stdin) are required")
	}
	// J1: build the body with json.Marshal, not %q, so a value containing
	// a control char (e.g. ESC 0x1b, vtab 0x0b) or a quote yields valid JSON
	// instead of a mangled, unparseable body.
	body, err := json.Marshal(secretWritePayload{
		OrgID: org, RepoID: repo, Name: name, Value: val,
	})
	if err != nil {
		return fmt.Errorf("secrets set: encoding request: %w", err)
	}
	status, out, err := doRequest(runCtx, http.MethodPost, server+"/api/v1/secrets", token, body)
	if err != nil {
		return fmt.Errorf("secrets set: %w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("secrets set: server returned %d: %s", status, string(out))
	}
	if _, err := fmt.Fprintf(os.Stdout, "set secret %s/%s\n", org, name); err != nil {
		return err
	}
	return nil
}

// cmdSecretDelete implements `cicado secrets delete --org O --repo R --name N`.
func cmdSecretDelete(args []string) error {
	fs := flag.NewFlagSet("secrets delete", flag.ContinueOnError)
	var (
		server string
		token  string
		org    string
		repo   string
		name   string
	)
	fs.StringVar(&server, "server", envOr("CICADO_SERVER", ""), "server base URL")
	fs.StringVar(&token, "token", envOr("CICADO_API_TOKEN", ""), "operator bearer token")
	fs.StringVar(&org, "org", "", "org id (required)")
	fs.StringVar(&repo, "repo", "", "repo id (org-scoped when empty)")
	fs.StringVar(&name, "name", "", "secret name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if org == "" || name == "" {
		return fmt.Errorf("secrets delete: --org and --name are required")
	}
	if server == "" {
		return fmt.Errorf("secrets delete: --server is required (or set CICADO_SERVER)")
	}
	q := url.Values{}
	q.Set("org_id", org)
	q.Set("name", name)
	if repo != "" {
		q.Set("repo_id", repo)
	}
	status, out, err := doRequest(runCtx, http.MethodDelete, server+"/api/v1/secrets?"+q.Encode(), token, nil)
	if err != nil {
		return fmt.Errorf("secrets delete: %w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("secrets delete: server returned %d: %s", status, string(out))
	}
	if _, err := fmt.Fprintf(os.Stdout, "deleted secret %s/%s\n", org, name); err != nil {
		return err
	}
	return nil
}

// cmdRun dispatches `cicado run <cancel> ...`.
func cmdRun(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("run: missing action; usage: cicado run cancel --reason R <jobID> | cicado run cancel-run --reason R <runID>")
	}
	switch args[0] {
	case "cancel":
		return cmdRunCancel(args[1:])
	case "cancel-run":
		return cmdRunCancelRun(args[1:])
	default:
		return fmt.Errorf("run: unknown action %q (try 'cancel', 'cancel-run')", args[0])
	}
}

// cmdRunCancel implements `cicado run cancel --reason R <jobID>`. Go's flag
// package stops at the first non-flag argument, so flags precede the job id.
func cmdRunCancel(args []string) error {
	fs := flag.NewFlagSet("run cancel", flag.ContinueOnError)
	var (
		server string
		token  string
		reason string
	)
	fs.StringVar(&server, "server", envOr("CICADO_SERVER", ""), "server base URL")
	fs.StringVar(&token, "token", envOr("CICADO_API_TOKEN", ""), "operator bearer token")
	fs.StringVar(&reason, "reason", "cancelled by operator via CLI", "cancel reason")
	if err := fs.Parse(args); err != nil {
		return err
	}
	remaining := fs.Args()
	if len(remaining) != 1 {
		return fmt.Errorf("run cancel: exactly one job id is required (after any flags)")
	}
	jobID := remaining[0]
	if server == "" {
		return fmt.Errorf("run cancel: --server is required (or set CICADO_SERVER)")
	}
	// J1: json.Marshal, not %q, so a reason with a control char is valid JSON.
	body, err := json.Marshal(cancelPayload{Reason: reason})
	if err != nil {
		return fmt.Errorf("run cancel: encoding request: %w", err)
	}
	status, out, err := doRequest(runCtx, http.MethodPost, server+"/api/v1/jobs/"+url.PathEscape(jobID)+"/cancel", token, body)
	if err != nil {
		return fmt.Errorf("run cancel: %w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("run cancel: server returned %d: %s", status, string(out))
	}
	if _, err := fmt.Fprintf(os.Stdout, "cancel requested for job %s\n", jobID); err != nil {
		return err
	}
	return nil
}

// cmdRunCancelRun implements `cicado run cancel-run --reason R <runID>`
// (CIC-206). It requests cancellation of every active job of the run
// via POST /api/v1/runs/{id}/cancel.
func cmdRunCancelRun(args []string) error {
	fs := flag.NewFlagSet("run cancel-run", flag.ContinueOnError)
	var (
		server string
		token  string
		reason string
	)
	fs.StringVar(&server, "server", envOr("CICADO_SERVER", ""), "server base URL")
	fs.StringVar(&token, "token", envOr("CICADO_API_TOKEN", ""), "operator bearer token")
	fs.StringVar(&reason, "reason", "cancelled by operator via CLI", "cancel reason")
	if err := fs.Parse(args); err != nil {
		return err
	}
	remaining := fs.Args()
	if len(remaining) != 1 {
		return fmt.Errorf("run cancel-run: exactly one run id is required (after any flags)")
	}
	runID := remaining[0]
	if server == "" {
		return fmt.Errorf("run cancel-run: --server is required (or set CICADO_SERVER)")
	}
	body, err := json.Marshal(cancelPayload{Reason: reason})
	if err != nil {
		return fmt.Errorf("run cancel-run: encoding request: %w", err)
	}
	status, out, err := doRequest(runCtx, http.MethodPost, server+"/api/v1/runs/"+url.PathEscape(runID)+"/cancel", token, body)
	if err != nil {
		return fmt.Errorf("run cancel-run: %w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("run cancel-run: server returned %d: %s", status, string(out))
	}
	if _, err := fmt.Fprintf(os.Stdout, "cancel requested for run %s\n", runID); err != nil {
		return err
	}
	return nil
}

// envOr returns the value of the named env var when set and non-empty, else def.
func envOr(env, def string) string {
	if v, ok := os.LookupEnv(env); ok && v != "" {
		return v
	}
	return def
}

// secretWritePayload is the JSON body for `cicado secrets set` -> POST
// /api/v1/secrets. Mirrors server/api's secretWriteBody.
type secretWritePayload struct {
	OrgID  string `json:"org_id"`
	RepoID string `json:"repo_id"`
	Name   string `json:"name"`
	Value  string `json:"value"`
}

// cancelPayload is the JSON body for `cicado run cancel` ->
// POST /api/v1/jobs/{id}/cancel. Mirrors server/api's cancelJobBody.
type cancelPayload struct {
	Reason string `json:"reason"`
}

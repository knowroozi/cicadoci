package api

// writes.go implements CIC-194's operator write surface: write-only secrets
// (PUT/DELETE) and job Cancel (POST .../cancel) routes. Each write handler is
// mounted on the SAME mux as every other /api/v1 route, so it shares buildChain
// (authenticate -> per-request timeout); it is AUDITED by a separate handler-level
// seam, auditWrite, that the write handlers call themselves. Before this,
// secrets.Put/Delete and queue.RequestCancel had no production caller, so the
// platform had no way to set/delete a secret or cancel a job.

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/audit"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/secrets"
)

// operatorActor is the audit actor id for an operator API write. M1 has no
// per-principal model (that is CIC-44), so the bearer-token operator is
// recorded as a single, clearly-named "api-operator" principal. CIC-44 will
// resolve a real principal id here. The actor is the operator surface, not a
// runner or system component, so it must never be a zero UUID or "system":
// CIC-192 made the audit principal-faithful, and a write attributed to
// "system" would indistinguish an operator action from a background one.
const operatorActor = "api-operator"

// WriteDeps is CIC-194's operator write surface, injected via WithWriteDeps.
// Each capability gates on its own nil check, so a server built without a
// secrets key or a wired cancel path exposes none of those routes at all
// (fail-closed by absence), and audit is a no-op when it is nil.
type WriteDeps struct {
	// SecretStore backs the secrets PUT/DELETE routes; nil -> routes absent.
	SecretStore secrets.Store
	// Cancel requests job cancellation (queue.RequestCancel); nil -> route absent.
	Cancel func(ctx context.Context, jobID uuid.UUID, reason string) (queue.CancelResult, error)
	// Audit is appended for every write; nil -> audits are no-ops.
	Audit audit.Log
}

// WithWriteDeps arms CIC-194's operator write routes through the same auth +
// timeout chain as every other /api/v1 route; the write handlers additionally
// self-audit via auditWrite. A capability whose dependency is nil is simply not
// registered, so a server without a CICADO_SECRET_KEY does not expose a broken
// secrets route and a server without a cancel path does not expose a broken
// cancel route.
func WithWriteDeps(d WriteDeps) Option {
	return func(o *options) { o.writes = d }
}

// secretWriteBody is the JSON body for POST /api/v1/secrets. The value is
// supplied here and never echoed back or logged; only the org/repo/name are
// recorded in the audit entry (the secret value is not), so the credential is
// not made readable through the write path or the audit stream.
type secretWriteBody struct {
	OrgID  string `json:"org_id"`
	RepoID string `json:"repo_id"`
	Name   string `json:"name"`
	Value  string `json:"value"`
}

// handlePutSecret serves POST /api/v1/secrets: it stores the secret under
// (org, repo, name) and writes an ActionSecretPut audit entry. The value is
// consumed and never returned. E1: a malformed org_id/repo_id is a 400 (the
// client's fault); every other store failure is a 500 with a generic message,
// never raw database text.
func (h *Handler) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	var body secretWriteBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "invalid secret body", err)
		return
	}
	if body.OrgID == "" || body.Name == "" || body.Value == "" {
		writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "org_id, name, and value are required", nil)
		return
	}
	// E1: validate the identifiers before touching the store so a malformed id is a
	// clean 400, and so no malformed value can reach the query text.
	if _, err := uuid.Parse(body.OrgID); err != nil {
		writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "org_id must be a valid uuid", nil)
		return
	}
	if body.RepoID != "" {
		if _, err := uuid.Parse(body.RepoID); err != nil {
			writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "repo_id must be a valid uuid", nil)
			return
		}
	}
	if err := h.writes.SecretStore.Put(r.Context(), body.OrgID, body.RepoID, body.Name, []byte(body.Value), nil); err != nil {
		// E1: a store failure is a server-side condition (DB down, constraint).
		// Report a 500 with a generic message; never surface err.Error() (which can
		// carry database text / schema detail) to the operator.
		h.logger.Error("api: secret put failed", "err", err)
		writeError(w, h.logger, http.StatusInternalServerError, errCodeInternal, "could not save secret", nil)
		return
	}
	h.auditWrite(r.Context(), audit.Entry{
		Action: audit.ActionSecretPut,
		Actor:  operatorActor,
		Target: secretTarget(body.OrgID, body.RepoID, body.Name),
		Reason: "secret set",
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteSecret serves DELETE /api/v1/secrets?org_id=&repo_id=&name= . It
// deletes the secret and writes an ActionSecretDelete audit entry. The delete
// is idempotent per secrets.Delete. E1 applies as in handlePutSecret.
func (h *Handler) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	orgID, repoID, name := q.Get("org_id"), q.Get("repo_id"), q.Get("name")
	if orgID == "" || name == "" {
		writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "org_id and name are required", nil)
		return
	}
	if _, err := uuid.Parse(orgID); err != nil {
		writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "org_id must be a valid uuid", nil)
		return
	}
	// repo_id is optional (org-scoped secrets); when present it must parse.
	if repoID != "" {
		if _, err := uuid.Parse(repoID); err != nil {
			writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "repo_id must be a valid uuid", nil)
			return
		}
	}
	if err := h.writes.SecretStore.Delete(r.Context(), orgID, repoID, name); err != nil {
		h.logger.Error("api: secret delete failed", "err", err)
		writeError(w, h.logger, http.StatusInternalServerError, errCodeInternal, "could not delete secret", nil)
		return
	}
	h.auditWrite(r.Context(), audit.Entry{
		Action: audit.ActionSecretDelete,
		Actor:  operatorActor,
		Target: secretTarget(orgID, repoID, name),
		Reason: "secret deleted",
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleListSecrets serves GET /api/v1/secrets?org_id=&repo_id= : it returns the
// NAMES (never the values) of the secrets in the org/repo scope, per
// secrets.Store.List. This is a read, but it rides the write surface's
// SecretStore (and thus its auth + fail-closed wiring), keeping one operator
// surface for the secrets API. E1 applies: a present org_id/repo_id must be a
// valid uuid.
func (h *Handler) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	orgID, repoID := q.Get("org_id"), q.Get("repo_id")
	if orgID == "" {
		writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "org_id is required", nil)
		return
	}
	if _, err := uuid.Parse(orgID); err != nil {
		writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "org_id must be a valid uuid", nil)
		return
	}
	if repoID != "" {
		if _, err := uuid.Parse(repoID); err != nil {
			writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "repo_id must be a valid uuid", nil)
			return
		}
	}
	names, err := h.writes.SecretStore.List(r.Context(), orgID, repoID)
	if err != nil {
		// E1: never surface raw database text to the operator.
		h.logger.Error("api: secret list failed", "err", err)
		writeError(w, h.logger, http.StatusInternalServerError, errCodeInternal, "could not list secrets", nil)
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"names": names})
}

// cancelJobBody is the JSON body for POST /api/v1/jobs/{id}/cancel.
type cancelJobBody struct {
	Reason string `json:"reason"`
}

// handleCancelJob serves POST /api/v1/jobs/{id}/cancel: it requests
// cancellation of the job (a runner that holds a lease sees it on its next
// heartbeat) and writes an ActionCancel audit entry. C1: when the job is
// unknown or already terminal, Cancel returns Active=false -- that is a 404 and
// NO audit row, because no cancellation happened; only a real in-flight cancel
// (Active=true) is audited, so a false "job cancelled" row is never written.
func (h *Handler) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	jobID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "invalid job id", nil)
		return
	}
	var body cancelJobBody
	// The body is optional (reason defaults); a decode error is only a signal
	// that a body was present but malformed.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "invalid cancel body", err)
			return
		}
	}
	reason := body.Reason
	if reason == "" {
		reason = "cancelled by operator via API"
	}
	res, err := h.writes.Cancel(r.Context(), jobID, reason)
	// R1a: a failed cancel IS a server-side condition, so pass err as
	// writeError's cause to log it server-side (writeError logs the cause
	// itself, so this is the single log point). No state changed, so the
	// failed attempt is not audited; only a real in-flight cancel is.
	if err != nil {
		writeError(w, h.logger, http.StatusInternalServerError, errCodeInternal, "could not request job cancel", err)
		return
	}
	// C1: a no-op cancel (job unknown or already finished) is not a state change,
	// so it is not audited and reports 404 rather than a misleading 200 + row.
	if !res.Active {
		writeError(w, h.logger, http.StatusNotFound, errCodeNotFound, "job not found or already finished", nil)
		return
	}
	h.auditWrite(r.Context(), audit.Entry{
		Action: audit.ActionCancel,
		Actor:  operatorActor,
		Target: jobID.String(),
		Reason: reason,
	})
	writeJSON(w, http.StatusOK, map[string]any{"active": res.Active, "runner_id": res.RunnerID.String()})
}

// secretTarget renders the (org, repo, name) identity for an audit Target
// without the secret value. A repo-scoped secret (RepoID set) names itself as
// "org/repo/name"; an org-scoped one omits the repo.
func secretTarget(orgID, repoID, name string) string {
	if repoID != "" {
		return orgID + "/" + repoID + "/" + name
	}
	return orgID + "/" + name
}

// auditWrite appends a write-surface audit entry, logging (but not failing the
// request on) an append error, so an operator write is never silently dropped.
// This is a handler-level seam the write handlers invoke; it is NOT a layer of
// buildChain (which is authenticate + per-request timeout). A nil audit log is a
// no-op.
func (h *Handler) auditWrite(ctx context.Context, e audit.Entry) {
	if h.writes.Audit == nil {
		return
	}
	if _, err := h.writes.Audit.Append(ctx, e); err != nil {
		h.logger.Error("api: audit append failed for operator write", "action", e.Action, "err", err)
	}
}

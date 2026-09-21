package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/store"
)

// defaultListLimit is the page size applied when a GET /runs request omits ?limit entirely. Until M3 adds real caller
// authentication (today the only gate on this surface is noOpAuthenticator, per handler.go), an uncapped list would let any
// unauthenticated client trigger a full-table scan for free -- an interim, deliberately conservative backstop that keeps that
// exposure bounded without changing behaviour for clients who pass an explicit ?limit value of their own. This constant will be
// revisited once real auth lands and the route stops being reachable anonymously; it lives here (not in handler.go's separate
// limit/timeout const block) purely to keep M1's list-surface defaults co-located with the one call site that consults them.
const defaultListLimit = 100

// listRuns serves GET /api/v1/runs: newest-first across every org, honoring ?status= and an optional numeric limit (capped
// here before it ever reaches the store's own clause). Malformed inputs get a 400 with errCodeBadRequest.
func (h *Handler) listRuns(w http.ResponseWriter, r *http.Request) {
	opts := store.ListRunsOptions{}

	if raw := r.URL.Query().Get("status"); raw != "" {
		opts.Status = raw
	}

	// ?limit caps a client's page size before it ever reaches the store's own query (which adds its own hidden +1 row to detect "more").
	// Because that sentinel fetch lives in the store layer already, we pass the request size through verbatim here rather than re-adding one.
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n < 0 || n > maxListLimit {
			writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "invalid limit: must be between 0 and "+strconv.Itoa(maxListLimit), nil)
			return
		}
		opts.Limit = n
	} else {
		// No explicit ?limit at all: backstop with a sensible default page so an unauthenticated M1 caller (noOpAuthenticator is the
		// only gate today, pre-M3 real auth) can't trigger a completely uncapped full scan of this table. This is a documented,
		// deliberately conservative interim guard -- it stays on the list surface itself until M3's authentication layer makes
		// caller identity an actual prerequisite for reaching store reads at all.
		opts.Limit = defaultListLimit
	}

	runs, more, err := h.reader.ListRuns(r.Context(), opts)
	if err != nil {
		writeError(w, h.logger, http.StatusInternalServerError, errCodeInternal, "could not list runs", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "has_more": more})
}

// getRun serves GET /api/v1/runs/{id}: a single run by its id path parameter. A malformed id is the client's error (400);
// a well-formed-but-missing one maps to store.ErrNotFound -> 404; anything else gets logged and returned as errCodeInternal.
func (h *Handler) getRun(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "invalid run id", nil)
		return
	}

	run, err := h.reader.GetRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, h.logger, http.StatusNotFound, errCodeNotFound, "no such run", err)
		return
	}
	if err != nil {
		writeError(w, h.logger, http.StatusInternalServerError, errCodeInternal, "could not load run", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/store"
)

// listRuns serves GET /api/v1/runs: newest-first across every org, honoring ?status= and an optional numeric limit (capped
// here before it ever reaches the store's own clause). Malformed inputs get a 400 with errCodeBadRequest.
func (h *Handler) listRuns(w http.ResponseWriter, r *http.Request) {
	opts := store.ListRunsOptions{}

	if raw := r.URL.Query().Get("status"); raw != "" {
		opts.Status = raw
	}

	// +1 to the requested cap; collectRuns returns up to that many runs plus flags "more" when a hidden extra row exists.
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n < 0 || n > maxListLimit {
			writeError(w, h.logger, http.StatusBadRequest, errCodeBadRequest, "invalid limit: must be between 0 and "+strconv.Itoa(maxListLimit), nil)
			return
		}
		opts.Limit = n + 1
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

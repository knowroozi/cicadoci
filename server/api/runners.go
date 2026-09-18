package api

import "net/http"

// listRunners serves GET /api/v1/runners: the current fleet, newest-registration first (the store orders by registered_at so
// operators see which runner just came online). Every org's runners are flat-listed here -- no ?org= filter in M1; GitHub
// OAuth + per-org scoping lands in M3/M5 and can slice this list without changing its shape.
func (h *Handler) listRunners(w http.ResponseWriter, r *http.Request) {
	runnerz, err := h.reader.ListRunners(r.Context())
	if err != nil {
		writeError(w, h.logger, http.StatusInternalServerError, errCodeInternal, "could not list runners", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"runners": runnerz})
}

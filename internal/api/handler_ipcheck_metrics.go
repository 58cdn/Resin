package api

import (
	"net/http"

	"github.com/Resinat/Resin/internal/metrics"
)

// HandleIPCheckDials handles GET /api/v1/metrics/ip-checks/dials. It only reads
// process-local dial counters; it never resolves a plugin or starts a check.
func HandleIPCheckDials(mgr *metrics.Manager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if mgr == nil {
			WriteError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "IP check dial metrics unavailable")
			return
		}
		// Inspect the raw query so malformed encodings and empty query markers
		// cannot silently turn an unsupported filter into a global snapshot.
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			WriteError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "query parameters are not supported")
			return
		}
		WriteJSON(w, http.StatusOK, mgr.SnapshotIPCheckDials())
	})
}

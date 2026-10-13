package api

import (
	"net/http"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/service"
)

type nodeIPCheckRequest struct {
	Sources []string `json:"sources"`
	Force   bool     `json:"force"`
}

type ipCheckLookupRequest struct {
	IP      string   `json:"ip"`
	Sources []string `json:"sources"`
	Force   bool     `json:"force"`
}

// writeIPCheckResult excludes cache metadata from public responses without
// mutating a result that may also be retained by the engine or store.
func writeIPCheckResult(w http.ResponseWriter, result *ipcheck.Result) {
	response := *result
	response.Sources = append([]ipcheck.SourceResult{}, result.Sources...)
	for i := range response.Sources {
		response.Sources[i].ConfigFingerprint = ""
	}
	WriteJSON(w, http.StatusOK, response)
}

// HandleCheckNodeIP handles POST /api/v1/nodes/{hash}/actions/check-ip.
func HandleCheckNodeIP(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body *nodeIPCheckRequest
		if err := DecodeBody(r, &body); err != nil {
			writeDecodeBodyError(w, err)
			return
		}
		if body == nil {
			writeInvalidArgument(w, "request body must be a JSON object")
			return
		}
		result, err := cp.CheckNodeIP(r.Context(), PathParam(r, "hash"), body.Sources, body.Force)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeIPCheckResult(w, result)
	}
}

// HandleGetNodeIPCheck handles GET /api/v1/nodes/{hash}/ip-check.
func HandleGetNodeIPCheck(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := cp.GetNodeIPCheck(r.Context(), PathParam(r, "hash"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeIPCheckResult(w, result)
	}
}

// HandleGetIPCheck handles GET /api/v1/ip-checks/{ip}.
func HandleGetIPCheck(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := cp.GetIPCheck(r.Context(), PathParam(r, "ip"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeIPCheckResult(w, result)
	}
}

// HandleLookupIPCheck handles POST /api/v1/ip-checks/lookup.
func HandleLookupIPCheck(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body *ipCheckLookupRequest
		if err := DecodeBody(r, &body); err != nil {
			writeDecodeBodyError(w, err)
			return
		}
		if body == nil {
			writeInvalidArgument(w, "request body must be a JSON object")
			return
		}
		result, err := cp.LookupIPCheck(r.Context(), body.IP, body.Sources, body.Force)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeIPCheckResult(w, result)
	}
}

// HandleListIPCheckSources handles GET /api/v1/ip-checks/sources.
func HandleListIPCheckSources(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sources, err := cp.ListIPCheckSources()
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"items": sources, "total": len(sources)})
	}
}

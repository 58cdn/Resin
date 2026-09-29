package api

import (
	"net/http"

	"github.com/Resinat/Resin/internal/service"
)

func HandleListPlugins(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pg, ok := parsePaginationOrWriteInvalid(w, r)
		if !ok {
			return
		}
		plugins, err := cp.ListPlugins()
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WritePage(w, http.StatusOK, plugins, pg)
	}
}

func HandleGetPlugin(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := cp.GetPlugin(PathParam(r, "id"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, info)
	}
}

func HandleUpdatePlugin(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := readRawBodyOrWriteInvalid(w, r)
		if !ok {
			return
		}
		info, err := cp.PatchPlugin(r.Context(), PathParam(r, "id"), body)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, info)
	}
}

func HandleDeletePlugin(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := cp.UninstallPlugin(r.Context(), PathParam(r, "id")); err != nil {
			writeServiceError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func HandleRescanPlugins(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		plugins, err := cp.RescanPlugins(r.Context())
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"items": plugins, "total": len(plugins)})
	}
}

// HandleUploadPlugin installs a plugin package sent as the raw request body
// (.zip or .tar.gz). It is mounted outside the default API body limit.
func HandleUploadPlugin(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := readRawBodyOrWriteInvalid(w, r)
		if !ok {
			return
		}
		info, err := cp.UploadPlugin(r.Context(), body)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusCreated, info)
	}
}

func HandlePluginMarketplace(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		listing, err := cp.PluginMarketplace(r.Context())
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, listing)
	}
}

func HandleInstallMarketplacePlugin(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := cp.InstallMarketplacePlugin(r.Context(), PathParam(r, "id"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, info)
	}
}

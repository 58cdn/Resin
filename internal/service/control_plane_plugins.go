package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Resinat/Resin/internal/plugin"
)

// ------------------------------------------------------------------
// Plugins
// ------------------------------------------------------------------

var pluginPatchAllowedFields = map[string]bool{
	"enabled":     true,
	"priority":    true,
	"timeout_ms":  true,
	"fail_closed": true,
	"config":      true,
}

func (s *ControlPlaneService) pluginManager() (*plugin.Manager, *ServiceError) {
	if s.Plugins == nil {
		return nil, conflict("plugin system is not available")
	}
	return s.Plugins, nil
}

// ListPlugins returns every known plugin (builtin and installed packages).
func (s *ControlPlaneService) ListPlugins() ([]plugin.Info, error) {
	mgr, svcErr := s.pluginManager()
	if svcErr != nil {
		return nil, svcErr
	}
	return mgr.List(), nil
}

// GetPlugin returns a single plugin.
func (s *ControlPlaneService) GetPlugin(id string) (plugin.Info, error) {
	mgr, svcErr := s.pluginManager()
	if svcErr != nil {
		return plugin.Info{}, svcErr
	}
	info, err := mgr.Get(id)
	if err != nil {
		return plugin.Info{}, pluginServiceError(err)
	}
	return info, nil
}

// PatchPlugin applies a constrained merge patch to plugin settings.
// The "config" field, when present, replaces the whole plugin config object.
func (s *ControlPlaneService) PatchPlugin(ctx context.Context, id string, patchJSON json.RawMessage) (plugin.Info, error) {
	mgr, svcErr := s.pluginManager()
	if svcErr != nil {
		return plugin.Info{}, svcErr
	}
	patch, svcErr := parseMergePatch(patchJSON)
	if svcErr != nil {
		return plugin.Info{}, svcErr
	}
	if svcErr := patch.validateFields(pluginPatchAllowedFields, func(field string) string {
		return "field is not patchable: " + field
	}); svcErr != nil {
		return plugin.Info{}, svcErr
	}

	var update plugin.Update
	if v, ok, svcErr := patch.optionalBool("enabled"); svcErr != nil {
		return plugin.Info{}, svcErr
	} else if ok {
		update.Enabled = &v
	}
	if v, ok, svcErr := patch.optionalInt("priority"); svcErr != nil {
		return plugin.Info{}, svcErr
	} else if ok {
		update.Priority = &v
	}
	if v, ok, svcErr := patch.optionalInt("timeout_ms"); svcErr != nil {
		return plugin.Info{}, svcErr
	} else if ok {
		update.TimeoutMs = &v
	}
	if v, ok, svcErr := patch.optionalBool("fail_closed"); svcErr != nil {
		return plugin.Info{}, svcErr
	} else if ok {
		update.FailClosed = &v
	}
	if raw, ok := patch["config"]; ok {
		if _, isObject := raw.(map[string]any); !isObject {
			return plugin.Info{}, invalidArg("config: must be an object")
		}
		// Re-read the raw bytes so numbers keep their original precision.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(patchJSON, &fields); err != nil {
			return plugin.Info{}, invalidArg("invalid JSON: " + err.Error())
		}
		update.Config = bytes.TrimSpace(fields["config"])
	}

	info, err := mgr.Update(ctx, id, update)
	if err != nil {
		return plugin.Info{}, pluginServiceError(err)
	}
	return info, nil
}

// UninstallPlugin removes an installed package plugin.
func (s *ControlPlaneService) UninstallPlugin(ctx context.Context, id string) error {
	mgr, svcErr := s.pluginManager()
	if svcErr != nil {
		return svcErr
	}
	if err := mgr.Uninstall(ctx, id); err != nil {
		return pluginServiceError(err)
	}
	return nil
}

// RescanPlugins re-reads the plugin directory.
func (s *ControlPlaneService) RescanPlugins(ctx context.Context) ([]plugin.Info, error) {
	mgr, svcErr := s.pluginManager()
	if svcErr != nil {
		return nil, svcErr
	}
	infos, err := mgr.Rescan(ctx)
	if err != nil {
		return nil, pluginServiceError(err)
	}
	return infos, nil
}

// PluginMarketplace lists plugins from the configured marketplace indexes.
func (s *ControlPlaneService) PluginMarketplace(ctx context.Context) (plugin.MarketplaceListing, error) {
	mgr, svcErr := s.pluginManager()
	if svcErr != nil {
		return plugin.MarketplaceListing{}, svcErr
	}
	listing, err := mgr.Marketplace(ctx)
	if err != nil {
		return plugin.MarketplaceListing{}, pluginServiceError(err)
	}
	return listing, nil
}

// InstallMarketplacePlugin downloads and installs (or upgrades) a plugin.
func (s *ControlPlaneService) InstallMarketplacePlugin(ctx context.Context, id string) (plugin.Info, error) {
	mgr, svcErr := s.pluginManager()
	if svcErr != nil {
		return plugin.Info{}, svcErr
	}
	info, err := mgr.Install(ctx, id)
	if err != nil {
		return plugin.Info{}, pluginServiceError(err)
	}
	return info, nil
}

// UploadPlugin installs a plugin package archive (.zip or .tar.gz).
func (s *ControlPlaneService) UploadPlugin(ctx context.Context, data []byte) (plugin.Info, error) {
	mgr, svcErr := s.pluginManager()
	if svcErr != nil {
		return plugin.Info{}, svcErr
	}
	if len(data) == 0 {
		return plugin.Info{}, invalidArg("empty plugin package")
	}
	info, err := mgr.InstallArchive(ctx, data)
	if err != nil {
		return plugin.Info{}, pluginServiceError(err)
	}
	return info, nil
}

// pluginServiceError maps plugin manager errors to service errors, dropping
// the sentinel prefix ("invalid argument: ...") from the message.
func pluginServiceError(err error) *ServiceError {
	msg := func(sentinel error) string {
		text := err.Error()
		if trimmed := strings.TrimPrefix(text, sentinel.Error()+": "); trimmed != "" {
			return trimmed
		}
		return text
	}
	switch {
	case errors.Is(err, plugin.ErrNotFound):
		return notFound(msg(plugin.ErrNotFound))
	case errors.Is(err, plugin.ErrInvalidArgument):
		return invalidArg(msg(plugin.ErrInvalidArgument))
	case errors.Is(err, plugin.ErrStartFailed):
		return invalidArg(err.Error())
	case errors.Is(err, plugin.ErrExternalDisabled):
		return conflict(err.Error())
	case errors.Is(err, plugin.ErrConflict):
		return conflict(msg(plugin.ErrConflict))
	default:
		return internal("plugin operation failed: "+err.Error(), err)
	}
}

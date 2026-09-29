package main

import (
	"context"
	"log"

	"github.com/Resinat/Resin/internal/buildinfo"
	"github.com/Resinat/Resin/internal/plugin"
	"github.com/Resinat/Resin/internal/plugin/builtin"
	"github.com/Resinat/Resin/internal/state"
)

// newPluginManager builds the plugin manager. It is created before the router
// so lease events can be forwarded from the first one; it is started later by
// startPlugins, once the proxies that call into it exist.
func (a *resinApp) newPluginManager(engine *state.StateEngine) *plugin.Manager {
	return plugin.NewManager(plugin.ManagerConfig{
		Store:           engine,
		PluginDir:       a.envCfg.PluginDir,
		ExternalEnabled: a.envCfg.ExternalPluginsEnabled,
		MarketplaceURLs: a.envCfg.PluginMarketplaceURLs,
		ResinVersion:    buildinfo.Version,
		PlatformName: func(id string) string {
			if a.topoRuntime == nil || a.topoRuntime.pool == nil {
				return ""
			}
			if plat, ok := a.topoRuntime.pool.GetPlatform(id); ok {
				return plat.Name
			}
			return ""
		},
		Builtins: builtin.All(),
	})
}

func (a *resinApp) startPlugins() error {
	if err := a.plugins.Start(context.Background()); err != nil {
		return err
	}
	if a.envCfg.ExternalPluginsEnabled {
		log.Printf("Plugin manager started (external plugins enabled, dir %s)", a.envCfg.PluginDir)
	} else {
		log.Println("Plugin manager started (builtin plugins only)")
	}
	return nil
}

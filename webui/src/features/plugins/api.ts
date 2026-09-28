import { apiRequest } from "../../lib/api-client";
import type {
  MarketplaceListing,
  MarketplacePlugin,
  Plugin,
  PluginConfig,
  PluginListResponse,
  PluginPatch,
  PluginStats,
} from "./types";

const basePath = "/api/v1/plugins";
const marketplacePath = "/api/v1/plugin-marketplace";

function normalizeStats(raw: Partial<PluginStats> | undefined): PluginStats {
  const n = (v: unknown) => (typeof v === "number" && Number.isFinite(v) ? v : 0);
  return {
    requests: n(raw?.requests),
    rejects: n(raw?.rejects),
    errors: n(raw?.errors),
    timeouts: n(raw?.timeouts),
    avg_latency_us: n(raw?.avg_latency_us),
    events_delivered: n(raw?.events_delivered),
    events_dropped: n(raw?.events_dropped),
    event_errors: n(raw?.event_errors),
    restarts: n(raw?.restarts),
  };
}

function normalizeConfig(raw: unknown): PluginConfig {
  if (raw && typeof raw === "object" && !Array.isArray(raw)) {
    return raw as PluginConfig;
  }
  return {};
}

function normalizePlugin(raw: Plugin): Plugin {
  return {
    id: raw.id || "",
    name: raw.name || raw.id || "",
    version: raw.version || "",
    description: raw.description || "",
    author: raw.author || "",
    homepage: raw.homepage || "",
    license: raw.license || "",
    source: raw.source === "package" ? "package" : "builtin",
    capabilities: {
      request_hook: Boolean(raw.capabilities?.request_hook),
      events: Array.isArray(raw.capabilities?.events) ? raw.capabilities.events : [],
    },
    config_fields: Array.isArray(raw.config_fields) ? raw.config_fields : [],
    enabled: Boolean(raw.enabled),
    priority: Number(raw.priority) || 0,
    timeout_ms: Number(raw.timeout_ms) || 0,
    fail_closed: Boolean(raw.fail_closed),
    config: normalizeConfig(raw.config),
    status: raw.status || "stopped",
    last_error: raw.last_error || "",
    stats: normalizeStats(raw.stats),
    created_at: raw.created_at || "",
    updated_at: raw.updated_at || "",
  };
}

function normalizeMarketplacePlugin(raw: MarketplacePlugin): MarketplacePlugin {
  return {
    ...raw,
    name: raw.name || raw.id,
    tags: Array.isArray(raw.tags) ? raw.tags : [],
    artifacts: Array.isArray(raw.artifacts) ? raw.artifacts : [],
    marketplace: raw.marketplace || "",
    installed_version: raw.installed_version || "",
    update_available: Boolean(raw.update_available),
    installable: Boolean(raw.installable),
    reason: raw.reason || "",
  };
}

export async function listPlugins(): Promise<Plugin[]> {
  const data = await apiRequest<PluginListResponse>(`${basePath}?limit=100000&offset=0`);
  return Array.isArray(data.items) ? data.items.map(normalizePlugin) : [];
}

export async function updatePlugin(id: string, patch: PluginPatch): Promise<Plugin> {
  const data = await apiRequest<Plugin>(`${basePath}/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: patch,
  });
  return normalizePlugin(data);
}

export async function uninstallPlugin(id: string): Promise<void> {
  await apiRequest<void>(`${basePath}/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export async function rescanPlugins(): Promise<Plugin[]> {
  const data = await apiRequest<{ items: Plugin[] }>(`${basePath}/actions/rescan`, {
    method: "POST",
  });
  return Array.isArray(data.items) ? data.items.map(normalizePlugin) : [];
}

export async function uploadPlugin(file: File): Promise<Plugin> {
  const data = await apiRequest<Plugin>(`${basePath}/actions/upload`, {
    method: "POST",
    rawBody: file,
  });
  return normalizePlugin(data);
}

export async function getMarketplace(): Promise<MarketplaceListing> {
  const data = await apiRequest<MarketplaceListing>(marketplacePath);
  return {
    sources: Array.isArray(data.sources) ? data.sources : [],
    plugins: Array.isArray(data.plugins) ? data.plugins.map(normalizeMarketplacePlugin) : [],
    errors: Array.isArray(data.errors) ? data.errors : [],
  };
}

export async function installMarketplacePlugin(id: string): Promise<Plugin> {
  const data = await apiRequest<Plugin>(`${marketplacePath}/${encodeURIComponent(id)}/actions/install`, {
    method: "POST",
  });
  return normalizePlugin(data);
}

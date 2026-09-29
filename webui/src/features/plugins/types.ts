import type { JsonValue } from "../../lib/api-client";

export type PluginSource = "builtin" | "package";

export type PluginStatus = "running" | "stopped" | "starting" | "error";

export type PluginConfigFieldType =
  | "string"
  | "secret"
  | "text"
  | "number"
  | "integer"
  | "boolean"
  | "enum"
  | "string_list"
  | "json";

export type PluginConfigField = {
  name: string;
  label?: string;
  type: PluginConfigFieldType;
  description?: string;
  required?: boolean;
  default?: JsonValue;
  enum_values?: string[];
};

export type PluginCapabilities = {
  request_hook?: boolean;
  events?: string[];
};

export type PluginStats = {
  requests: number;
  rejects: number;
  errors: number;
  timeouts: number;
  avg_latency_us: number;
  events_delivered: number;
  events_dropped: number;
  event_errors: number;
  restarts: number;
};

export type PluginConfig = { [key: string]: JsonValue };

export type Plugin = {
  id: string;
  name: string;
  version: string;
  description: string;
  author: string;
  homepage: string;
  license: string;
  source: PluginSource;
  capabilities: PluginCapabilities;
  config_fields: PluginConfigField[];
  enabled: boolean;
  priority: number;
  timeout_ms: number;
  fail_closed: boolean;
  config: PluginConfig;
  status: PluginStatus;
  last_error: string;
  stats: PluginStats;
  created_at: string;
  updated_at: string;
};

export type PluginListResponse = {
  items: Plugin[];
  total: number;
  limit: number;
  offset: number;
};

export type PluginPatch = {
  enabled?: boolean;
  priority?: number;
  timeout_ms?: number;
  fail_closed?: boolean;
  config?: PluginConfig;
};

export type MarketplaceArtifact = {
  os: string;
  arch: string;
  url: string;
  sha256: string;
  size?: number;
};

export type MarketplacePlugin = {
  id: string;
  name: string;
  version: string;
  description?: string;
  author?: string;
  homepage?: string;
  license?: string;
  tags?: string[];
  min_resin_version?: string;
  artifacts: MarketplaceArtifact[];
  marketplace: string;
  installed_version: string;
  update_available: boolean;
  installable: boolean;
  reason?: string;
};

export type MarketplaceError = {
  url: string;
  error: string;
};

export type MarketplaceListing = {
  sources: string[];
  plugins: MarketplacePlugin[];
  errors: MarketplaceError[];
};

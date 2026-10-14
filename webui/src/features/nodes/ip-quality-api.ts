import { apiRequest } from "../../lib/api-client";
import type { IPQualityResult, IPQualitySourceStatus } from "./ip-quality";

// These functions only read the cache and catalog. Do not call check/lookup POSTs
// when opening, refreshing, or changing nodes in the details panel.
export function getNodeIPQuality(hash: string, signal?: AbortSignal): Promise<IPQualityResult> {
  return apiRequest(`/api/v1/nodes/${encodeURIComponent(hash)}/ip-check`, { signal });
}

export async function listIPQualitySources(signal?: AbortSignal): Promise<IPQualitySourceStatus[]> {
  const result = await apiRequest<{ items: IPQualitySourceStatus[] }>("/api/v1/ip-checks/sources", { signal });
  return result.items;
}

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

// This POST only reads existing cache. It is deliberately separate from the
// check-ip and lookup endpoints, which can perform external observations.
export async function readNodeIPQualityBatch(hashes: string[], signal: AbortSignal): Promise<unknown> {
  const request = new AbortController();
  const abort = () => request.abort(signal.reason);
  signal.addEventListener("abort", abort, { once: true });
  if (signal.aborted) abort();
  const timer = setTimeout(() => request.abort(new DOMException("Cache read timed out", "TimeoutError")), 20_000);
  try {
    return await apiRequest("/api/v1/ip-checks/cache/nodes", { method: "POST", body: { node_hashes: hashes }, signal: request.signal });
  } catch (error) {
    if (request.signal.aborted) throw request.signal.reason;
    throw error;
  } finally {
    clearTimeout(timer);
    signal.removeEventListener("abort", abort);
  }
}

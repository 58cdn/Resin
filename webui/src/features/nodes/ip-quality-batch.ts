import type { NodeSummary } from "./types";

export type QualityNode = Pick<NodeSummary, "node_hash" | "egress_ip" | "last_egress_update">;
export type QualityCacheEntry = {
  kind: "cached" | "partial" | "sources_failed" | "empty" | "conflict" | "too_large" | "failed" | "mismatch" | "expired";
  sourceCount: number;
  expiresAt: number | null;
  ipType: string;
};
export type BatchReader = (hashes: string[], signal: AbortSignal) => Promise<unknown>;

const emptyEntry = (kind: QualityCacheEntry["kind"]): QualityCacheEntry => ({ kind, sourceCount: 0, expiresAt: null, ipType: "unknown" });
const record = (value: unknown): value is Record<string, unknown> => value !== null && typeof value === "object" && !Array.isArray(value);

// Retain only badge facts; do not keep up to 5,000 full source payloads in a list.
// The detail panel remains the place to inspect original source observations.
function compactResult(value: unknown, node: QualityNode, now: number): QualityCacheEntry {
  if (!record(value) || typeof value.egress_ip !== "string" || !Array.isArray(value.sources) || !record(value.summary)) return emptyEntry("failed");
  if (!node.egress_ip || value.egress_ip !== node.egress_ip) return emptyEntry("mismatch");
  if (!value.sources.length) return emptyEntry("empty");
  let expiresAt = Infinity;
  let successful = 0;
  for (const source of value.sources) {
    if (!record(source) || typeof source.expires_at !== "string" || typeof source.ok !== "boolean" || typeof source.status !== "string") return emptyEntry("failed");
    const expiry = Date.parse(source.expires_at);
    if (!Number.isFinite(expiry) || expiry <= now) return emptyEntry("expired");
    expiresAt = Math.min(expiresAt, expiry);
    if (source.ok && source.status === "ok") successful++;
  }
  return {
    kind: successful === value.sources.length ? "cached" : successful ? "partial" : "sources_failed",
    sourceCount: value.sources.length,
    expiresAt,
    ipType: typeof value.summary.ip_type === "string" ? value.summary.ip_type : "unknown",
  };
}

export function compactBatchResponse(value: unknown, nodes: QualityNode[], now: number): Record<string, QualityCacheEntry> {
  if (!record(value) || !Array.isArray(value.items) || value.total !== nodes.length || value.items.length !== nodes.length) throw new Error("Invalid batch cache response");
  const requested = new Map(nodes.map((node) => [node.node_hash, node]));
  const entries: Record<string, QualityCacheEntry> = {};
  for (const item of value.items) {
    if (!record(item) || typeof item.node_hash !== "string" || !requested.has(item.node_hash) || Object.hasOwn(entries, item.node_hash)) throw new Error("Invalid batch cache identity");
    const hasResult = Object.hasOwn(item, "result");
    const hasError = Object.hasOwn(item, "error");
    if (hasResult === hasError) throw new Error("Ambiguous batch cache item");
    if (hasError) {
      const code = record(item.error) ? item.error.code : undefined;
      entries[item.node_hash] = emptyEntry(code === "NOT_FOUND" ? "empty" : code === "CONFLICT" ? "conflict" : code === "RESULT_TOO_LARGE" ? "too_large" : "failed");
    } else {
      entries[item.node_hash] = compactResult(item.result, requested.get(item.node_hash)!, now);
    }
  }
  return entries;
}

export function nextBatchExpiryDelay(entries: Record<string, QualityCacheEntry>, renderedAt: number, currentTime: number): number | null {
  let next = Infinity;
  for (const entry of Object.values(entries)) if (entry.expiresAt !== null && entry.expiresAt > renderedAt) next = Math.min(next, entry.expiresAt);
  return Number.isFinite(next) ? Math.min(Math.max(0, next - currentTime + 1), 2_147_483_647) : null;
}

function pause(signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    signal.throwIfAborted();
    const timer = setTimeout(() => { signal.removeEventListener("abort", abort); resolve(); }, 600);
    const abort = () => { clearTimeout(timer); reject(signal.reason); };
    signal.addEventListener("abort", abort, { once: true });
  });
}

export async function readIPQualityBatches(nodes: QualityNode[], signal: AbortSignal, read: BatchReader, onBatch: (entries: Record<string, QualityCacheEntry>) => void): Promise<void> {
  const unique = [...new Map(nodes.map((node) => [node.node_hash, node])).values()];
  for (let offset = 0; offset < unique.length; offset += 50) {
    signal.throwIfAborted();
    if (offset) await pause(signal); // Below the route's two requests/second budget; no retry loop.
    signal.throwIfAborted();
    const chunk = unique.slice(offset, offset + 50);
    const response = await read(chunk.map((node) => node.node_hash), signal);
    signal.throwIfAborted();
    onBatch(compactBatchResponse(response, chunk, Date.now()));
  }
}

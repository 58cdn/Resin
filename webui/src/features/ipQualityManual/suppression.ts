export type SuppressionReason = "source_rate" | "source_daily_budget" | "source_circuit_open" | "source_half_open_busy";
export type SuppressedSource = {
  source: "egress-consistency" | "cloudflare-challenge" | "ai-region";
  kind: "via_node";
  admitted: number;
  suppressed: number;
  reasons: { reason: SuppressionReason; count: number; retry_at?: string }[];
};

const sources = ["egress-consistency", "cloudflare-challenge", "ai-region"];
const reasons = ["source_rate", "source_daily_budget", "source_circuit_open", "source_half_open_busy"];
const object = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const count = (value: unknown, minimum: number): value is number => typeof value === "number" && Number.isInteger(value) && value >= minimum && value <= 8;

export function observationTimestamp(value: unknown): value is string {
  if (typeof value !== "string" || value.length > 40) return false;
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d{1,9})?(Z|[+-]\d{2}:\d{2})$/.exec(value);
  if (!match) return false;
  const [, year, month, day, hour, minute, second, zone] = match;
  const y = Number(year), m = Number(month), d = Number(day);
  const days = [31, y % 4 === 0 && (y % 100 !== 0 || y % 400 === 0) ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return y > 0 && m >= 1 && m <= 12 && d >= 1 && d <= days[m - 1] && Number(hour) <= 23 && Number(minute) <= 59 && Number(second) <= 59
    && (zone === "Z" || (Number(zone.slice(1, 3)) <= 23 && Number(zone.slice(4)) <= 59)) && Number.isFinite(Date.parse(value));
}

/** Fixed source/reason vocabulary and bounded counts; never contains targets. */
export function parseSuppressions(value: unknown): SuppressedSource[] | null {
  if (value === undefined) return [];
  if (!Array.isArray(value) || value.length > 3) return null;
  const result: SuppressedSource[] = [];
  for (const raw of value) {
    if (!object(raw) || typeof raw.source !== "string" || !sources.includes(raw.source) || raw.kind !== "via_node"
      || result.some(source => source.source === raw.source) || !count(raw.admitted, 0) || !count(raw.suppressed, 1)
      || raw.admitted + raw.suppressed > 8 || !Array.isArray(raw.reasons) || !raw.reasons.length || raw.reasons.length > 4) return null;
    const parsed: SuppressedSource = { source: raw.source as SuppressedSource["source"], kind: "via_node", admitted: raw.admitted, suppressed: raw.suppressed, reasons: [] };
    for (const entry of raw.reasons) {
      if (!object(entry) || typeof entry.reason !== "string" || !reasons.includes(entry.reason) || !count(entry.count, 1)
        || parsed.reasons.some(reason => reason.reason === entry.reason)
        || (entry.retry_at !== undefined && (!observationTimestamp(entry.retry_at) || entry.reason === "source_half_open_busy"))) return null;
      parsed.reasons.push({ reason: entry.reason as SuppressionReason, count: entry.count, ...(entry.retry_at !== undefined ? { retry_at: entry.retry_at as string } : {}) });
    }
    if (parsed.reasons.reduce((sum, reason) => sum + reason.count, 0) !== parsed.suppressed) return null;
    result.push(parsed);
  }
  return result;
}

export const suppressionLabels: Record<SuppressionReason, string> = {
  source_rate: "来源请求速率限制", source_daily_budget: "来源当日预算已用尽",
  source_circuit_open: "来源保护冷却中", source_half_open_busy: "来源试探尚未结束",
};

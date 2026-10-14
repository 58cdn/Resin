import type { IPQualityResult } from "../nodes/ip-quality";

// Validate literals before building the path: "sources" is a different API.
// Public-address policy remains authoritative on the server (400).
export function normalizeCacheIP(input: string): string | null {
  const value = input.trim();
  if (!value || value.length > 45 || !/^[\da-f:.]+$/i.test(value)) return null;
  const ipv4 = (text: string) => /^(0|[1-9]\d{0,2})(\.(0|[1-9]\d{0,2})){3}$/.test(text) && text.split(".").every(part => Number(part) <= 255);
  if (!value.includes(":")) return ipv4(value) ? value : null;
  if (value.includes(".") && !ipv4(value.slice(value.lastIndexOf(":") + 1))) return null;
  try {
    const ip = new URL(`http://[${value}]/`).hostname.slice(1, -1);
    const mapped = /^::ffff:([\da-f]+):([\da-f]+)$/.exec(ip);
    if (mapped) {
      const high = parseInt(mapped[1], 16), low = parseInt(mapped[2], 16);
      return [high >> 8, high & 255, low >> 8, low & 255].join(".");
    }
    return ip;
  } catch { return null; }
}

const object = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

export function isIPCacheResult(value: unknown, ip: string): value is IPQualityResult {
  if (!object(value) || typeof value.egress_ip !== "string" || normalizeCacheIP(value.egress_ip) !== ip || typeof value.checked_at !== "string" || !object(value.summary) || !Array.isArray(value.sources)) return false;
  const summary = value.summary;
  if (["country", "city", "as_org", "ip_type", "ip_origin", "risk_level"].some(key => summary[key] != null && typeof summary[key] !== "string")) return false;
  if (["asn", "fraud_score"].some(key => summary[key] != null && (typeof summary[key] !== "number" || !Number.isFinite(summary[key])))) return false;
  if (summary.abuse_flags != null && (!Array.isArray(summary.abuse_flags) || summary.abuse_flags.some(flag => typeof flag !== "string"))) return false;
  const names = new Set<string>();
  return value.sources.every(source => {
    if (!object(source) || typeof source.source !== "string" || !source.source || names.has(source.source) || source.kind !== "by_ip" || typeof source.ok !== "boolean" || typeof source.status !== "string" || typeof source.checked_at !== "string" || typeof source.expires_at !== "string" || !object(source.data) || (source.error !== undefined && typeof source.error !== "string")) return false;
    names.add(source.source);
    return true;
  });
}

export function ipCacheStatus(result: IPQualityResult, now: number) {
  if (!result.sources.length) return "empty";
  if (result.sources.some(source => !(Date.parse(source.expires_at) > now))) return "expired";
  const successful = result.sources.filter(source => source.ok && source.status === "ok").length;
  if (successful === result.sources.length) return "cached";
  if (successful) return "partial";
  return result.sources.every(source => source.status === "unavailable") ? "unavailable" : "failed";
}

export function cacheReadErrorKey(status?: number): string {
  if (status === 400) return "此地址不受支持，请输入有效的公网 IPv4 或 IPv6 地址。";
  if (status === 404) return "暂无有效缓存：可能尚未检测、缓存已过期或配置已变更。";
  if (status === 401 || status === 403) return "无权读取 IP 质量结果，请检查管理员登录状态。";
  if (status === 429) return "读取过于频繁，请稍后手动重试。";
  if (status === 503) return "IP 质量插件未启用或暂不可用。";
  return "读取 IP 质量缓存失败，请重试。";
}

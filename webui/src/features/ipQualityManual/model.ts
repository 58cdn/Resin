import type { IPQualityResult, IPQualitySourceStatus } from "../nodes/ip-quality";

export type SubjectKind = "ip" | "node";
export type NodeIdentity = { hash: string; ip: string; version: string; name: string };
export type PreparedCheck = { kind: SubjectKind; value: string; ip: string; node?: NodeIdentity; sources: IPQualitySourceStatus[] };
export const CHECK_WAIT_MS = 130_000;
export const READ_WAIT_MS = 20_000;
const kinds: Record<string, string> = { "db-ip": "by_ip", "egress-consistency": "via_node", "cloudflare-challenge": "via_node", "ai-region": "via_node", ippure: "via_node", scamalytics: "by_ip", ipdata: "by_ip", "cloudflare-radar": "by_ip" };
const supported = new Set(["db-ip", "egress-consistency", "cloudflare-challenge", "ai-region"]);
const object = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

export function normalizeManualIP(input: string): string | null {
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

export function normalizeSubject(kind: SubjectKind, value: string): string | null {
  return kind === "ip" ? normalizeManualIP(value) : /^[\da-f]{32}$/i.test(value.trim()) ? value.trim().toLowerCase() : null;
}

export function nodeIdentity(value: unknown, hash: string): NodeIdentity | null {
  if (!object(value) || value.node_hash !== hash || typeof value.egress_ip !== "string" || (value.last_egress_update != null && typeof value.last_egress_update !== "string")) return null;
  const ip = normalizeManualIP(value.egress_ip);
  if (!ip) return null;
  return { hash, ip, version: (value.last_egress_update as string) || "", name: typeof value.display_tag === "string" ? value.display_tag : "" };
}

export function sameNode(left: NodeIdentity, right: NodeIdentity | null): boolean {
  return !!right && left.hash === right.hash && left.ip === right.ip && left.version === right.version;
}

export function sourceCatalog(value: unknown): IPQualitySourceStatus[] | null {
  if (!object(value) || !Array.isArray(value.items)) return null;
  const names = new Set<string>();
  for (const source of value.items) {
    if (!object(source) || typeof source.name !== "string" || !source.name || names.has(source.name) || (source.kind !== "via_node" && source.kind !== "by_ip") || typeof source.enabled !== "boolean" || typeof source.available !== "boolean" || typeof source.requires_key !== "boolean" || (source.reason !== undefined && typeof source.reason !== "string") || (source.attribution !== undefined && typeof source.attribution !== "string")) return null;
    if (Object.hasOwn(kinds, source.name) && kinds[source.name] !== source.kind) return null;
    names.add(source.name);
  }
  return value.items as IPQualitySourceStatus[];
}

export function selectable(source: IPQualitySourceStatus, kind: SubjectKind): boolean {
  return supported.has(source.name) && source.enabled && source.available && !source.requires_key && (kind === "node" || source.kind === "by_ip");
}

export function validSelection(prepared: PreparedCheck, names: string[]): boolean {
  return names.length > 0 && new Set(names).size === names.length && names.every(name => prepared.sources.some(source => source.name === name && selectable(source, prepared.kind)));
}

export function isManualResult(value: unknown, prepared: PreparedCheck, names?: string[]): value is IPQualityResult {
  if (!object(value) || typeof value.egress_ip !== "string" || normalizeManualIP(value.egress_ip) !== prepared.ip || typeof value.checked_at !== "string" || !Number.isFinite(Date.parse(value.checked_at)) || !object(value.summary) || !Array.isArray(value.sources)) return false;
  const summary = value.summary;
  if (["country", "city", "as_org", "ip_type", "ip_origin", "risk_level", "cf_challenge"].some(key => summary[key] != null && typeof summary[key] !== "string")) return false;
  if (["asn", "fraud_score"].some(key => summary[key] != null && (typeof summary[key] !== "number" || !Number.isFinite(summary[key])))) return false;
  if (summary.egress_consistent != null && typeof summary.egress_consistent !== "boolean") return false;
  if (summary.abuse_flags != null && (!Array.isArray(summary.abuse_flags) || summary.abuse_flags.some(flag => typeof flag !== "string"))) return false;
  if (["services", "egress_ips_by_site"].some(key => summary[key] != null && (!object(summary[key]) || Object.values(summary[key]).some(item => typeof item !== "string")))) return false;
  const seen = new Set<string>();
  for (const source of value.sources) {
    if (!object(source) || typeof source.source !== "string" || !Object.hasOwn(kinds, source.source) || kinds[source.source] !== source.kind || (prepared.kind === "ip" && source.kind !== "by_ip") || seen.has(source.source) || typeof source.ok !== "boolean" || typeof source.status !== "string" || typeof source.checked_at !== "string" || !Number.isFinite(Date.parse(source.checked_at)) || typeof source.expires_at !== "string" || !(Date.parse(source.expires_at) > Date.parse(source.checked_at)) || !object(source.data) || (source.error !== undefined && typeof source.error !== "string") || (source.egress_ip !== undefined && (typeof source.egress_ip !== "string" || normalizeManualIP(source.egress_ip) !== prepared.ip))) return false;
    seen.add(source.source);
  }
  return names === undefined || (seen.size === names.length && names.every(name => seen.has(name)));
}

export function resultStatus(result: IPQualityResult, now: number): string {
  if (!result.sources.length) return "未返回来源观测，不能判断检测通过。";
  if (result.sources.some(source => !(Date.parse(source.expires_at) > now))) return "部分观测已过期，已隐藏摘要和过期数据。";
  const count = result.sources.filter(source => source.ok && source.status === "ok").length;
  if (count === result.sources.length) return "已返回来源观测；不代表节点健康或账户可用。";
  if (count) return "部分来源失败，请查看逐来源原因。";
  return result.sources.every(source => source.status === "unavailable") ? "所选来源不可用，未取得有效观测。" : "所有来源均未成功，请查看逐来源原因。";
}

export function manualErrorKey(status?: number, cache = false): string {
  if (status === 400) return "地址、节点或来源选择无效，请重新准备。";
  if (status === 401 || status === 403) return "无权执行此操作，请检查管理员登录状态。";
  if (status === 404) return cache ? "暂无有效缓存：可能尚未检测、缓存已过期或对象已移除。" : "节点或对象不存在，请重新准备。";
  if (status === 409) return "节点出口不可用或已变化，请刷新对象；不会自动探测出口。";
  if (status === 429) return "服务端限制了请求频率，请稍后手动重试。";
  if (status === 503) return "插件不可用、检测繁忙或请求已超时，请检查配置后手动重试。";
  return "操作失败，无法确认检测是否完成；可手动读取缓存核对。";
}

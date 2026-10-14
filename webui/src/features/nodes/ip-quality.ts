export type IPQualitySourceResult = {
  source: string;
  kind: "via_node" | "by_ip";
  ok: boolean;
  status: string;
  checked_at: string;
  expires_at: string;
  error?: string;
  data: Record<string, unknown>;
};

export type IPQualityResult = {
  egress_ip: string;
  checked_at: string;
  summary: {
    country: string;
    city: string;
    asn: number | null;
    as_org: string;
    ip_type: string;
    ip_origin: string;
    fraud_score: number | null;
    abuse_flags: string[] | null;
    cf_challenge: string;
    egress_consistent: boolean | null;
    egress_ips_by_site: Record<string, string> | null;
    services: Record<string, string> | null;
    risk_level: string;
  };
  sources: IPQualitySourceResult[];
};

export type IPQualitySourceStatus = {
  name: string;
  kind: "via_node" | "by_ip";
  enabled: boolean;
  available: boolean;
  requires_key: boolean;
  reason?: string;
  attribution?: string;
};

export function isFreshSource(source: IPQualitySourceResult, now: number): boolean {
  return Date.parse(source.expires_at) > now;
}

export function nextExpiryDelay(sources: IPQualitySourceResult[], renderedAt: number, currentTime: number): number | null {
  // Include any deadline that was fresh at render time, even if it expired
  // before the effect ran. Otherwise a delayed effect could leave stale UI forever.
  const next = Math.min(...sources.map((source) => Date.parse(source.expires_at)).filter((expiry) => expiry > renderedAt));
  return Number.isFinite(next) ? Math.min(Math.max(0, next - currentTime + 1), 2_147_483_647) : null;
}

export function cacheMatchesNode(result: IPQualityResult, egressIP: string): boolean {
  return Boolean(egressIP) && result.egress_ip === egressIP;
}

// Keys are user-facing translation keys. Unknown enum values stay unknown;
// they must not imply a low risk, consistent egress or service availability.
const valueLabels: Record<string, string> = {
  residential: "住宅", datacenter: "数据中心", mobile: "移动网络",
  native: "原生", broadcast: "广播",
  none: "未发现挑战", challenge: "发现挑战", blocked: "被阻止",
  available: "可用观测", region_blocked: "地区受限", risky: "存在风险信号",
  low: "低", medium: "中", high: "高",
};

export function qualityValueLabel(value: string | undefined): string {
  return value && Object.hasOwn(valueLabels, value) ? valueLabels[value] : "未知";
}

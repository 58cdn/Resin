import { createRoot } from "react-dom/client";
import { flushSync } from "react-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, Link } from "react-router-dom";
import i18n from "../src/i18n";
import { ManualCheckRoute } from "../src/features/ipQualityManual/ManualCheckPage";
import { GeoIPPage } from "../src/features/geoip/GeoIPPage";
import { NodeIPQualityPanel } from "../src/features/nodes/NodeIPQualityPanel";
import "../src/styles/theme.css";
import "@fontsource/manrope/400.css";
import "@fontsource/sora/600.css";

const root = createRoot(document.getElementById("root")!);
const hash = "a".repeat(32);
let scenario = "success", nodeReads = 0, saved: unknown = null;
let client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
const requests: { path: string; method: string; body: Record<string, unknown>; cache?: RequestCache; aborted: boolean }[] = [];
const forbidden: string[] = [];
// Keep real timers by default. Tests may explicitly fire a recorded client wait
// deadline; this verifies scheduling/cleanup without spending 130 wall seconds.
const nativeTimeout = window.setTimeout.bind(window), nativeClear = window.clearTimeout.bind(window);
const deadlines = new Map<number, { ms: number; fire: () => void }>();
window.setTimeout = ((handler: TimerHandler, ms?: number, ...args: unknown[]) => {
  const id = nativeTimeout(handler, ms, ...args);
  if ((ms === 20_000 || ms === 130_000) && typeof handler === "function") deadlines.set(id, { ms, fire: () => { nativeClear(id); deadlines.delete(id); handler(...args); } });
  return id;
}) as typeof window.setTimeout;
window.clearTimeout = (id?: number) => { if (id !== undefined) deadlines.delete(id); nativeClear(id); };
const catalog = [
  { name: "db-ip", kind: "by_ip", enabled: true, available: true, requires_key: false },
  { name: "egress-consistency", kind: "via_node", enabled: true, available: true, requires_key: false },
  { name: "ai-region", kind: "via_node", enabled: false, available: false, requires_key: false, reason: "Fixture unconfigured target" },
  { name: "ipdata", kind: "by_ip", enabled: false, available: false, requires_key: true, reason: "Fixture keyed source unavailable" },
];
function result(ip: string, names: string[], current: string) {
  const checked_at = new Date(Date.now() - 60000).toISOString();
  const expires_at = new Date(Date.now() + (current === "expired" ? -1000 : current === "expiry" ? 1200 : 3600000)).toISOString();
  const summary = { country: "US", city: current === "late" ? "Old response" : "Fixture city", asn: null, as_org: "unknown", ip_type: "unknown", ip_origin: "unknown", fraud_score: 0, abuse_flags: [], cf_challenge: "unknown", egress_consistent: false, egress_ips_by_site: {}, services: {}, risk_level: "unknown" };
  if (current === "suppressed" || current === "partial-suppression") {
    const observed = names.filter(source => source === "db-ip");
    return { egress_ip: ip, ...(observed.length ? { checked_at } : {}), summary: { ...summary, egress_consistent: null },
      sources: observed.map(source => ({ source, kind: "by_ip", ok: true, status: "ok", checked_at, expires_at, data: { country: "US" } })),
      suppressed_sources: [{ source: "egress-consistency", kind: "via_node", admitted: current === "partial-suppression" ? 1 : 0, suppressed: 2, reasons: [{ reason: "source_daily_budget", count: 2, retry_at: "2026-10-15T00:00:00Z" }] }] };
  }
  return {
    egress_ip: current === "wrong-ip" ? "9.9.9.9" : ip, checked_at,
    summary: current === "malformed-summary" ? { ...summary, ip_type: { toString: null } } : summary,
    sources: current === "empty" ? [] : names.map((source, index) => {
      const ok = !["failed", "unavailable"].includes(current) && !(current === "partial" && index > 0);
      return { source: current === "wrong-source" ? "ipdata" : source, kind: source === "db-ip" ? "by_ip" : "via_node", ok, status: current === "unavailable" ? "unavailable" : ok ? "ok" : "error", checked_at, expires_at, egress_ip: ip, data: { city: summary.city, score: 0, flag: false, attribution: "Fixture attribution" }, ...(!ok ? { error: "<b>Fixture source timeout</b>" } : {}) };
    }),
  };
}

// Never fall back to a network backend, node, key or external target.
window.fetch = async (input, init) => {
  const url = new URL(String(input), location.origin), path = decodeURIComponent(url.pathname);
  const method = init?.method || "GET", body = init?.body ? JSON.parse(String(init.body)) : {};
  const record = { path, method, body, cache: init?.cache, aborted: false };
  requests.push(record);
  const node = path === `/api/v1/nodes/${hash}`;
  const sources = path === "/api/v1/ip-checks/sources";
  const post = method === "POST" && (path === "/api/v1/ip-checks/lookup" || path === `/api/v1/nodes/${hash}/actions/check-ip`);
  const cache = method === "GET" && (/^\/api\/v1\/ip-checks\/[^/]+$/.test(path) || path === `/api/v1/nodes/${hash}/ip-check`) && !sources;
  if (url.origin !== location.origin || !(method === "GET" && (node || sources || cache || path === "/api/v1/geoip/status") || post)) { forbidden.push(`${method} ${url}`); throw new Error("Forbidden fixture request"); }
  const current = scenario;
  if (node) nodeReads++;
  const readNumber = nodeReads;
  async function wait(ms: number, ignoreAbort = false) {
    await new Promise<void>((resolve, reject) => {
      const abort = () => { record.aborted = true; if (!ignoreAbort) { clearTimeout(timer); reject(init?.signal?.reason); } };
      const timer = setTimeout(() => { init?.signal?.removeEventListener("abort", abort); resolve(); }, ms);
      init?.signal?.addEventListener("abort", abort, { once: true });
      if (init?.signal?.aborted) abort();
    });
  }
  const delay = current === "late" && post ? 700 : (current === "pending" || current === "timeout") && post || current === "prepare-pending" && sources || current === "verify-pending" && node && readNumber >= 3 ? 300000 : 10;
  await wait(delay, current === "late");
  if (current === "network" && post) throw new TypeError("Fixture offline");
  const postStatus = ({ "bad": 400, "unauthorized": 401, "denied": 403, "missing": 404, "conflict": 409, "limited": 429, "busy": 503, "server-error": 500 } as Record<string, number>)[current];
  const status = sources && current === "disabled" ? 503 : sources && current === "catalog-denied" ? 403 : post && postStatus ? postStatus : cache && !saved ? 404 : 200;
  let payload: unknown;
  if (sources) payload = current === "bad-catalog" ? { items: [{ ...catalog[0], enabled: "true" }] } : { items: current === "no-sources" ? catalog.map(source => ({ ...source, enabled: false })) : catalog };
  else if (node) payload = { node_hash: hash, display_tag: "Fixture node", egress_ip: current === "no-egress" ? "" : current === "changed-before" && readNumber >= 2 || current === "changed-after" && readNumber >= 3 ? "9.9.9.9" : "8.8.8.8", last_egress_update: current === "version-after" && readNumber >= 3 ? "2026-10-14T10:01:00Z" : "2026-10-14T10:00:00Z" };
  else if (post) {
    payload = result(typeof body.ip === "string" ? body.ip : "8.8.8.8", body.sources as string[], current);
    // Models partial/cache mutation even if the final HTTP response fails.
    if (!["suppressed", "partial-suppression"].includes(current)) saved = payload;
  } else if (cache) payload = saved;
  else payload = { db_mtime: "", next_scheduled_update: "" };
  const response = new Response(current === "non-json" && post ? "fixture html" : JSON.stringify(status === 200 ? payload : { error: { code: "FIXTURE", message: "Fixture error" } }), { status, headers: { "Content-Type": current === "non-json" && post ? "text/html" : "application/json" } });
  if (current === "body-timeout" && post) response.json = async () => { await wait(300000); return payload; };
  return response;
};
const fixture = {
  requests, forbidden, hash,
  deadlines() { return [...deadlines.values()].map(item => item.ms); },
  expireWait(ms: number) { const item = [...deadlines.values()].find(item => item.ms === ms); if (!item) throw new Error("No matching client wait deadline"); item.fire(); },
  async scenario(value: string, entry = "/resources/ip-quality-check") {
    flushSync(() => root.render(null)); client.clear();
    client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    requests.length = 0; scenario = value; nodeReads = 0; saved = null;
    await i18n.changeLanguage("en-US");
    flushSync(() => root.render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[entry]}><Routes>
      <Route path="/resources/ip-quality-check" element={<ManualCheckRoute />} />
      <Route path="/resources" element={<GeoIPPage />} />
      <Route path="/nodes" element={<NodeIPQualityPanel nodeHash={hash} egressIP="8.8.8.8" egressVersion="2026-10-14T10:00:00Z" active={false} />} />
      <Route path="/plugins" element={<p>Fixture plugins <Link to="/resources/ip-quality-check">Return to manual check</Link></p>} />
    </Routes></MemoryRouter></QueryClientProvider>));
  },
  setScenario(value: string) { scenario = value; },
  unmount() { flushSync(() => root.render(null)); client.clear(); },
  locale(value: string) { return i18n.changeLanguage(value); },
};
Object.assign(window, { fixture });
void fixture.scenario("success");

import { createRoot } from "react-dom/client";
import { flushSync } from "react-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18n from "../src/i18n";
import { IPQualityCachePage } from "../src/features/ipQuality/IPQualityCachePage";
import { GeoIPPage } from "../src/features/geoip/GeoIPPage";
import "../src/styles/theme.css";
import "@fontsource/manrope/400.css";
import "@fontsource/sora/600.css";

const root = createRoot(document.getElementById("root")!);
let scenario = "success";
let client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
const requests: { path: string; method: string; cache?: RequestCache; aborted: boolean }[] = [];
const forbidden: string[] = [];
function result(ip: string, current: string) {
  const checked_at = new Date(Date.now() - 60000).toISOString();
  const expires_at = current === "invalid-date" ? "invalid" : new Date(Date.now() + (current === "expired" ? -1000 : ["expiry", "partial-expiry"].includes(current) ? 1200 : 3600000)).toISOString();
  const ok = current !== "unavailable" && current !== "failed";
  const summary = { country: "US", city: current === "late" ? "Old snapshot" : "Fixture city", asn: null, as_org: "unknown", ip_type: "unknown", ip_origin: "unknown", fraud_score: 0, abuse_flags: [], cf_challenge: "unknown", egress_consistent: null, egress_ips_by_site: {}, services: {}, risk_level: "unknown" };
  return {
    egress_ip: current === "wrong-ip" ? "9.9.9.9" : ip, checked_at, summary: current === "malformed" ? null : current === "malformed-summary" ? { ...summary, ip_type: { toString: null } } : summary,
    sources: current === "empty-200" ? [] : [
      { source: "db-ip", kind: current === "via-node" ? "via_node" : "by_ip", ok, status: current === "unavailable" ? "unavailable" : ok ? "ok" : "error", checked_at, expires_at, data: { city: summary.city, flag: false, score: 0, attribution: "Fixture attribution" }, ...(!ok ? { error: "Fixture source unavailable" } : {}) },
      ...(["partial", "partial-expiry"].includes(current) ? [{ source: "ipdata", kind: "by_ip", ok: false, status: "error", checked_at, expires_at: new Date(Date.now() + 3600000).toISOString(), error: "<b>Fixture source timeout</b>", data: { flag: false, score: 0 } }] : []),
    ],
  };
}

// All API traffic is intercepted. Never fall back to the network, a node, or a key.
window.fetch = async (input, init) => {
  const url = new URL(String(input), location.origin), path = decodeURIComponent(url.pathname);
  const method = init?.method || "GET";
  const record = { path, method, cache: init?.cache, aborted: false };
  requests.push(record);
  if (url.origin !== location.origin || method !== "GET" || !(/^\/api\/v1\/ip-checks\/[^/]+$/.test(path) || path === "/api/v1/geoip/status")) {
    forbidden.push(`${method} ${url}`); throw new Error("Forbidden fixture request");
  }
  const current = scenario;
  const catalog = path.endsWith("/sources");
  const delay = current === "late" && !catalog ? 650 : ["pending", "timeout"].includes(current) && !catalog || current === "catalog-pending" && catalog ? 60000 : 15;
  async function wait(ms: number, ignoreAbort = false) {
    await new Promise<void>((resolve, reject) => {
      const abort = () => { record.aborted = true; if (!ignoreAbort) { clearTimeout(timer); reject(init?.signal?.reason); } };
      const timer = setTimeout(() => { init?.signal?.removeEventListener("abort", abort); resolve(); }, ms);
      init?.signal?.addEventListener("abort", abort, { once: true });
      if (init?.signal?.aborted) abort();
    });
  }
  await wait(delay, current === "late");
  if (current === "network" && !catalog) throw new TypeError("Fixture offline");
  const status = catalog ? current === "catalog-disabled" ? 503 : current === "catalog-denied" ? 403 : current === "catalog-error" ? 500 : 200
    : ({ "missing": 404, "invalid-public": 400, "unauthorized": 401, "denied": 403, "limited": 429, "disabled": 503, "server-error": 500 } as Record<string, number>)[current] ?? 200;
  const body = catalog ? { items: [{ name: "db-ip", kind: "by_ip", enabled: true, available: true, requires_key: false }, { name: "egress-consistency", kind: "via_node", enabled: true, available: true, requires_key: false }] }
    : path === "/api/v1/geoip/status" ? { db_mtime: "", next_scheduled_update: "" } : result(path.split("/").at(-1)!, current);
  const response = new Response(current === "non-json" && !catalog ? "fixture html" : JSON.stringify(status === 200 ? body : { error: { code: "FIXTURE", message: "Fixture error" } }), { status, headers: { "Content-Type": current === "non-json" ? "text/html" : "application/json" } });
  if (current === "body-timeout" && !catalog) response.json = async () => { await wait(60000); return body; };
  return response;
};
const fixture = {
  requests, forbidden,
  async scenario(value: string, entry = "/resources/ip-quality") {
    flushSync(() => root.render(null)); client.clear();
    client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    requests.length = 0; scenario = value;
    await i18n.changeLanguage("en-US");
    flushSync(() => root.render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[entry]}><Routes>
      <Route path="/resources/ip-quality" element={<IPQualityCachePage />} />
      <Route path="/resources" element={<GeoIPPage />} />
      <Route path="/plugins" element={<p>Fixture plugins</p>} />
    </Routes></MemoryRouter></QueryClientProvider>));
  },
  setScenario(value: string) { scenario = value; },
  unmount() { flushSync(() => root.render(null)); client.clear(); },
  locale(value: string) { return i18n.changeLanguage(value); },
};
Object.assign(window, { fixture });
void fixture.scenario("success");

import { createRoot } from "react-dom/client";
import { flushSync } from "react-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import i18n from "../src/i18n";
import { NodesPage } from "../src/features/nodes/NodesPage";
import "../src/styles/theme.css";
import "@fontsource/manrope/400.css";
import "@fontsource/sora/600.css";

const root = createRoot(document.getElementById("root")!);
let scenario = "mixed";
let version = 1;
let client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
const requests: { path: string; method: string; hashes: string[]; at: number; aborted: boolean }[] = [];
const forbidden: string[] = [];
const batchPath = "/api/v1/ip-checks/cache/nodes";
const nodes = (offset = 0) => Array.from({ length: 70 }, (_, index) => ({
  node_hash: String(index + offset).padStart(32, "0"), display_tag: `Fixture node ${index + offset}`,
  created_at: "2026-01-01T00:00:00Z", enabled: true, has_outbound: true, failure_count: 0,
  egress_ip: "203.0.113.5", last_egress_update: `2026-01-01T00:00:0${version}Z`, region: "us", tags: [],
}));
const summary = { country: "US", city: "Fixture city", asn: null, as_org: "unknown", ip_type: "unknown", ip_origin: "unknown", fraud_score: null, abuse_flags: [], cf_challenge: "unknown", egress_consistent: null, egress_ips_by_site: {}, services: {}, risk_level: "unknown" };
function result(hash: string, current: string) {
  const index = Number(hash);
  const checked_at = new Date(Date.now() - 60000).toISOString();
  const expires_at = new Date(Date.now() + (current === "expiry" ? 1000 : 3600000)).toISOString();
  const ok = index !== 6;
  return {
    egress_ip: index === 5 && current === "mixed" ? "203.0.113.6" : "203.0.113.5", checked_at, summary,
    sources: [
      { source: "db-ip", kind: "by_ip", ok, status: ok ? "ok" : "error", checked_at, expires_at, data: { city: "Fixture city" } },
      ...(index === 1 ? [{ source: "egress-consistency", kind: "via_node", ok: false, status: "error", checked_at, expires_at, error: "Fixture timeout" }] : []),
    ],
  };
}

// Never fall back to native fetch. A new probe, lookup or unexpected endpoint fails the test.
window.fetch = async (input, init) => {
  const url = new URL(String(input), location.origin);
  const path = url.pathname;
  const method = init?.method || "GET";
  const record = { path, method, hashes: path === batchPath ? JSON.parse(String(init?.body)).node_hashes : [], at: Date.now(), aborted: false };
  requests.push(record);
  const allowed = url.origin === location.origin && (method === "POST" && path === batchPath || method === "GET" && /^\/api\/v1\/(nodes(?:\/[^/]+(?:\/ip-check)?)?|platforms|subscriptions|ip-checks\/sources)$/.test(path));
  if (!allowed) { forbidden.push(`${method} ${url}`); throw new Error("Unexpected fixture request"); }
  const current = scenario;
  const batchIndex = requests.filter(request => request.path === batchPath).length;
  const delay = path === batchPath && ["pending", "timeout"].includes(current) ? 60000 : path === batchPath && current === "cancel-second" && batchIndex === 2 ? 60000 : 20;
  await new Promise<void>((resolve, reject) => {
    const abort = () => { record.aborted = true; clearTimeout(timer); reject(init?.signal?.reason); };
    const timer = setTimeout(() => { init?.signal?.removeEventListener("abort", abort); resolve(); }, delay);
    init?.signal?.addEventListener("abort", abort, { once: true });
    if (init?.signal?.aborted) abort();
  });
  let body: unknown;
  let status = 200;
  if (path === batchPath) {
    status = current === "limited" ? 429 : current === "disabled" ? 503 : current === "unauthorized" ? 403 : current === "server-error" ? 500 : current === "disabled-second" && batchIndex === 2 ? 503 : 200;
    body = { total: record.hashes.length, items: record.hashes.map((node_hash: string) => {
      const error = current === "mixed" ? ({ 2: "NOT_FOUND", 3: "CONFLICT", 4: "RESULT_TOO_LARGE", 7: "INTERNAL" } as Record<number, string>)[Number(node_hash)] : undefined;
      return error ? { node_hash, error: { code: error, message: "Fixture error" } } : { node_hash, result: result(node_hash, current) };
    }) };
  } else if (path === "/api/v1/nodes") {
    const offset = Number(url.searchParams.get("offset") || 0);
    body = { items: nodes(offset), total: 400, limit: Number(url.searchParams.get("limit")), offset, unique_egress_ips: 1, unique_healthy_egress_ips: 1 };
  } else if (path.endsWith("/sources")) {
    body = { items: [{ name: "db-ip", kind: "by_ip", enabled: true, available: true, requires_key: false }], total: 1 };
  } else if (path.endsWith("/ip-check")) {
    body = result(path.split("/").at(-2)!, current);
  } else if (path.startsWith("/api/v1/nodes/")) {
    body = { ...nodes()[0], node_hash: path.split("/").at(-1) };
  } else {
    body = { items: [], total: 0, limit: 50, offset: 0 };
  }
  return new Response(JSON.stringify(status === 200 ? body : { error: { code: "FIXTURE", message: "Fixture error" } }), { status, headers: { "Content-Type": "application/json" } });
};

const fixture = {
  requests, forbidden,
  async scenario(value: string) {
    flushSync(() => root.render(null));
    client.clear();
    client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    requests.length = 0;
    scenario = value;
    version = 1;
    await i18n.changeLanguage("en-US");
    flushSync(() => root.render(<QueryClientProvider client={client}><MemoryRouter><NodesPage /></MemoryRouter></QueryClientProvider>));
  },
  setScenario(value: string) { scenario = value; },
  changeVersion() { version++; return client.invalidateQueries({ queryKey: ["nodes"] }); },
  unmount() { flushSync(() => root.render(null)); client.clear(); },
  locale(value: string) { return i18n.changeLanguage(value); },
};
Object.assign(window, { fixture });
void fixture.scenario("mixed");

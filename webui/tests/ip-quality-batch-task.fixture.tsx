import { createRoot } from "react-dom/client";
import { flushSync } from "react-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, Link } from "react-router-dom";
import i18n from "../src/i18n";
import { BatchTaskPage } from "../src/features/ipQualityBatch/BatchTaskPage";
import { ManualCheckRoute } from "../src/features/ipQualityManual/ManualCheckPage";
import "../src/styles/theme.css";
import "@fontsource/manrope/400.css";
import "@fontsource/sora/600.css";

const root = createRoot(document.getElementById("root")!);
const hash = "a".repeat(32), otherHash = "b".repeat(32), taskID = "1".repeat(32), otherID = "2".repeat(32);
type Envelope = { request_id: string; node_hashes: string[]; sources: string[]; force: boolean };
let scenario = "success", activeReads = 0, maxReads = 0;
let client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
const requests: { path: string; method: string; body: Record<string, unknown>; cache?: RequestCache; aborted: boolean }[] = [];
const forbidden: string[] = [];
const saved = new Map<string, Envelope>();
const nativeTimeout = window.setTimeout.bind(window), nativeClear = window.clearTimeout.bind(window);
const deadlines = new Map<number, { ms: number; fire: () => void }>();
window.setTimeout = ((handler: TimerHandler, ms?: number, ...args: unknown[]) => {
  const id = nativeTimeout(handler, ms, ...args);
  if ([2000, 20000, 300000].includes(ms ?? 0) && typeof handler === "function") deadlines.set(id, { ms: ms!, fire: () => { nativeClear(id); deadlines.delete(id); handler(...args); } });
  return id;
}) as typeof window.setTimeout;
window.clearTimeout = (id?: number) => { if (id !== undefined) deadlines.delete(id); nativeClear(id); };
const catalog = [
  { name: "db-ip", kind: "by_ip", enabled: true, available: true, requires_key: false },
  { name: "egress-consistency", kind: "via_node", enabled: true, available: true, requires_key: false },
  { name: "ai-region", kind: "via_node", enabled: false, available: false, requires_key: false },
  { name: "ipdata", kind: "by_ip", enabled: false, available: false, requires_key: true },
];
const created = "2026-10-13T10:00:00Z", started = "2026-10-13T10:00:01Z", finished = "2026-10-13T10:00:02Z";
const observed = "2026-10-12T10:00:00Z", expires = "2026-10-12T10:10:00Z";
function task(id: string, envelope: Envelope, state: string, mode: string) {
  const items = envelope.node_hashes.map((node_hash, index) => {
    if (state === "running") return index ? { node_hash, state: "queued" } : { node_hash, state: "running", started_at: started };
    if (state === "canceled") return { node_hash, state: "canceled", finished_at: finished };
    if (mode === "failed-item") return { node_hash, state: "failed", started_at: started, finished_at: finished, error: { code: "NOT_FOUND", message: "<b>Fixture node missing</b>" } };
    const sources = mode === "empty" ? [] : envelope.sources.map((source, sourceIndex) => {
      const ok = mode !== "all-failed" && mode !== "unavailable" && !(mode === "partial" && sourceIndex > 0);
      return { source, kind: source === "db-ip" ? "by_ip" : "via_node", ok, status: mode === "unavailable" ? "unavailable" : ok ? "ok" : "error", checked_at: observed, expires_at: expires, ...(!ok ? { error: "<b>Fixture source failed</b>" } : {}) };
    });
    return { node_hash, state: "completed", started_at: started, finished_at: finished, egress_ip: "8.8.8.8", checked_at: observed,
      observation_status: mode === "empty" ? "no_observations" : mode === "all-failed" ? "all_failed" : mode === "partial" ? "partial_failure" : mode === "unavailable" ? "unavailable" : "all_ok", sources };
  });
  const progress: Record<string, number> = { total: items.length, queued: 0, running: 0, completed: 0, failed: 0, canceled: 0 };
  for (const item of items) progress[item.state]++;
  return { id, request_id: envelope.request_id, state, sources: envelope.sources, force: envelope.force, created_at: created,
    ...(state !== "running" ? { finished_at: finished } : {}), ...(state === "canceled" ? { cancel_reason: "requested" } : {}), progress, items };
}

// This fixture never falls back to a Resin backend, real node or external target.
window.fetch = async (input, init) => {
  const url = new URL(String(input), location.origin), path = decodeURIComponent(url.pathname), method = init?.method || "GET";
  const body = init?.body ? JSON.parse(String(init.body)) : {};
  const record = { path, method, body, cache: init?.cache, aborted: false }; requests.push(record);
  const catalogRead = path === "/api/v1/ip-checks/sources" && method === "GET";
  const submit = path === "/api/v1/ip-checks/actions/check-batch" && method === "POST";
  const match = /^\/api\/v1\/ip-checks\/tasks\/([a-f\d]{32})(\/actions\/cancel)?$/.exec(path);
  const cancel = !!match?.[2] && method === "POST", read = !!match && !match[2] && method === "GET";
  if (url.origin !== location.origin || !(catalogRead || submit || read || cancel)) { forbidden.push(`${method} ${url}`); throw new Error("Forbidden fixture request"); }
  const mode = scenario;
  async function wait(ms: number, ignoreAbort = false) {
    await new Promise<void>((resolve, reject) => {
      const abort = () => { record.aborted = true; if (!ignoreAbort) { nativeClear(timer); reject(init?.signal?.reason); } };
      const timer = nativeTimeout(() => { init?.signal?.removeEventListener("abort", abort); resolve(); }, ms);
      init?.signal?.addEventListener("abort", abort, { once: true }); if (init?.signal?.aborted) abort();
    });
  }
  if (read) { activeReads++; maxReads = Math.max(maxReads, activeReads); }
  try {
    await wait(read && mode === "poll-late" || submit && mode === "submit-late" ? 700 : submit && mode === "pending" || catalogRead && mode === "prepare-pending" ? 600000 : 10, mode.endsWith("-late"));
    const id = match?.[1] ?? taskID;
    if (submit) saved.set(taskID, body as Envelope);
    if (submit && mode === "network") throw new TypeError("Fixture acceptance response lost");
    const statuses: Record<string, number> = { bad: 400, unauthorized: 401, denied: 403, missing: 404, conflict: 409, limited: 429, busy: 503, server: 500 };
    const status = !catalogRead && statuses[mode] ? statuses[mode] : submit ? 202 : 200;
    let payload: unknown = catalogRead ? { items: mode === "no-sources" ? catalog.map(source => ({ ...source, enabled: false })) : catalog } : task(id, saved.get(id)!, cancel ? "canceled" : submit || ["running", "poll-late", "body-timeout"].includes(mode) ? "running" : "completed", mode);
    if (mode === "wrong-id" && !catalogRead) payload = { ...(payload as object), id: otherID };
    if (mode === "wrong-scope" && !catalogRead) payload = task(id, { ...saved.get(id)!, node_hashes: [otherHash] }, "completed", "success");
    if (mode === "bad-catalog" && catalogRead) payload = { items: [{ ...catalog[0], enabled: "true" }] };
    const response = new Response(JSON.stringify(status < 300 ? payload : { error: { code: "FIXTURE", message: "Fixture failure" } }), { status, headers: { "Content-Type": "application/json", "Retry-After": "1" } });
    if (mode === "body-timeout" && read) response.json = async () => { await wait(600000); return payload; };
    return response;
  } finally { if (read) activeReads--; }
};
const fixture = {
  requests, forbidden, hash, otherHash, taskID, otherID,
  deadlines() { return [...deadlines.values()].map(item => item.ms); },
  expire(ms: number) { const item = [...deadlines.values()].find(item => item.ms === ms); if (!item) throw new Error("No matching timer"); item.fire(); },
  maxReads() { return maxReads; },
  async scenario(value: string, entry = "/resources/ip-quality-batch") {
    flushSync(() => root.render(null)); client.clear(); client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    requests.length = 0; scenario = value; saved.clear(); activeReads = 0; maxReads = 0;
    saved.set(taskID, { request_id: "restored_a", node_hashes: [hash], sources: ["db-ip"], force: false });
    saved.set(otherID, { request_id: "restored_b", node_hashes: [otherHash], sources: ["db-ip"], force: true });
    await i18n.changeLanguage("en-US");
    flushSync(() => root.render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[entry]}><Routes>
      <Route path="/resources/ip-quality-batch" element={<BatchTaskPage />} />
      <Route path="/resources/ip-quality-check" element={<ManualCheckRoute />} />
      <Route path="/plugins" element={<p>Fixture plugins <Link to="/resources/ip-quality-batch">Return to batch</Link></p>} />
      <Route path="/nodes" element={<p>Fixture nodes</p>} />
    </Routes></MemoryRouter></QueryClientProvider>));
  },
  setScenario(value: string) { scenario = value; },
  unmount() { flushSync(() => root.render(null)); client.clear(); },
  locale(value: string) { return i18n.changeLanguage(value); },
};
Object.assign(window, { fixture });
void fixture.scenario("success");

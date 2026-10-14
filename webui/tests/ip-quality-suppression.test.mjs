import assert from "node:assert/strict";
import test from "node:test";
import { parseSuppressions } from "../src/features/ipQualityManual/suppression.ts";
import { isManualResult, resultStatus } from "../src/features/ipQualityManual/model.ts";
import { parseBatchTask } from "../src/features/ipQualityBatch/model.ts";

const checked = "2026-10-14T15:00:00Z", expires = "2026-10-14T16:00:00Z";
const suppression = () => ({ source: "egress-consistency", kind: "via_node", admitted: 1, suppressed: 2, reasons: [{ reason: "source_daily_budget", count: 2, retry_at: "2026-10-15T00:00:00Z" }] });
const observation = () => ({ source: "db-ip", kind: "by_ip", ok: true, status: "ok", checked_at: checked, expires_at: expires, data: {} });
const prepared = { kind: "node", value: "a".repeat(32), ip: "8.8.8.8", sources: [] };
const result = () => ({ egress_ip: "8.8.8.8", sources: [], summary: {}, suppressed_sources: [suppression()] });
const item = () => ({ node_hash: "a".repeat(32), state: "completed", started_at: checked, finished_at: expires, egress_ip: "8.8.8.8", observation_status: "suppressed", suppressed_sources: [suppression()] });
const task = (entry = item(), names = ["egress-consistency"]) => ({ id: "1".repeat(32), request_id: "fixed", state: "completed", created_at: checked, finished_at: expires, force: true, sources: names, items: [entry], progress: { total: 1, queued: 0, running: 0, completed: 1, failed: 0, canceled: 0 } });

test("suppression parser bounds fixed vocabulary, per-attempt counts and retry timestamps", () => {
  assert.deepEqual(parseSuppressions([suppression()]), [suppression()]);
  for (const change of [{ source: "db-ip" }, { source: "https://secret.example" }, { kind: "by_ip" }, { admitted: -1 }, { admitted: 7 }, { suppressed: 0 }, { suppressed: 9 }, { reasons: [] }, { reasons: [{ reason: "provider_bill", count: 2 }] }, { reasons: [{ reason: "source_daily_budget", count: 1 }] }, { reasons: [{ reason: "source_daily_budget", count: 2, retry_at: "2026-02-30T00:00:00Z" }] }, { reasons: [{ reason: "source_half_open_busy", count: 2, retry_at: checked }] }]) {
    assert.equal(parseSuppressions([{ ...suppression(), ...change }]), null, JSON.stringify(change));
  }
  assert.equal(parseSuppressions([suppression(), suppression()]), null);
  assert.equal(parseSuppressions(null), null);
  assert.deepEqual(parseSuppressions(undefined), []);
  const original = [suppression()], copy = parseSuppressions(original); copy[0].reasons[0].count = 1;
  assert.equal(original[0].reasons[0].count, 2);
});

test("fully suppressed manual response has no observation time and still covers the exact selection", () => {
  const value = result();
  assert.equal(isManualResult(value, prepared, ["egress-consistency"]), true);
  assert.match(resultStatus(value, Date.parse(checked)), /没有新的完整来源观测/);
  for (const checked_at of [checked, "0001-01-01T00:00:00Z", null]) assert.equal(isManualResult({ ...value, checked_at }, prepared, ["egress-consistency"]), false);
  assert.equal(isManualResult(value, prepared, ["db-ip"]), false);
  assert.equal(isManualResult(value, { ...prepared, kind: "ip" }, ["egress-consistency"]), false);
  assert.equal(isManualResult(value, prepared), false); // Cache GET never returns controls.
});

test("mixed manual result separates observed and suppressed sources and their timestamps", () => {
  const value = { ...result(), checked_at: checked, sources: [observation()] };
  assert.equal(isManualResult(value, prepared, ["db-ip", "egress-consistency"]), true);
  assert.match(resultStatus(value, Date.parse(checked)), /部分来源受到本地限制/);
  assert.equal(isManualResult({ ...value, checked_at: expires }, prepared, ["db-ip", "egress-consistency"]), false);
  assert.equal(isManualResult(value, prepared, ["db-ip"]), false);
  assert.equal(isManualResult({ ...value, sources: [{ ...observation(), source: "egress-consistency", kind: "via_node" }] }, prepared, ["egress-consistency"]), false);
});

test("suppressed sources cannot contribute concrete facts through a malformed summary", () => {
  for (const [source, changes] of [
    ["egress-consistency", [{ egress_consistent: true }, { egress_consistent: false }, { egress_ips_by_site: { target: "8.8.8.8" } }]],
    ["cloudflare-challenge", [{ cf_challenge: "none" }, { cf_challenge: "challenge" }]],
    ["ai-region", [{ services: { chatgpt: "available" } }, { services: { claude: "risky" } }]],
  ]) {
    const value = { ...result(), checked_at: checked, sources: [observation()], suppressed_sources: [{ ...suppression(), source }] };
    assert.equal(isManualResult(value, prepared, ["db-ip", source]), true);
    for (const summary of changes) assert.equal(isManualResult({ ...value, summary }, prepared, ["db-ip", source]), false, JSON.stringify(summary));
  }
});

test("batch suppression completes lifecycle without claiming source failure or observation", () => {
  const parsed = parseBatchTask(task());
  assert.ok(parsed);
  assert.equal(parsed.items[0].checked_at, undefined);
  assert.equal(parsed.items[0].observation_status, "suppressed");
  assert.equal(parsed.progress.completed, 1); assert.equal(parsed.progress.failed, 0);
  for (const change of [{ checked_at: checked }, { observation_status: "all_ok" }, { observation_status: "all_failed" }, { error: { code: "INTERNAL", message: "guard" } }]) assert.equal(parseBatchTask(task({ ...item(), ...change })), null);
  assert.equal(parseBatchTask(task(item(), ["db-ip"])), null);
});

test("batch partial suppression requires complete disjoint selection and genuine aggregate time", () => {
  const { data: _, ...source } = observation();
  const entry = { ...item(), sources: [source], checked_at: checked, observation_status: "partial_suppression" };
  assert.ok(parseBatchTask(task(entry, ["db-ip", "egress-consistency"])));
  assert.equal(parseBatchTask(task(entry)), null);
  assert.equal(parseBatchTask(task({ ...entry, checked_at: expires }, ["db-ip", "egress-consistency"])), null);
  assert.equal(parseBatchTask(task({ ...entry, sources: [{ ...source, source: "egress-consistency", kind: "via_node" }] })), null);
});

test("unfinished and failed batch items cannot carry a hidden suppression result", () => {
  for (const state of ["queued", "running", "failed", "canceled"]) {
    const entry = { node_hash: "a".repeat(32), state, suppressed_sources: [suppression()], ...(state !== "queued" ? { started_at: checked } : {}), ...(["failed", "canceled"].includes(state) ? { finished_at: expires } : {}) };
    assert.equal(parseBatchTask(task(entry)), null);
  }
});

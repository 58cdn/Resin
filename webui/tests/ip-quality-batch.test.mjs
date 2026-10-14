import assert from "node:assert/strict";
import test from "node:test";
import { compactBatchResponse, nextBatchExpiryDelay, readIPQualityBatches } from "../src/features/nodes/ip-quality-batch.ts";

const now = Date.parse("2026-10-14T12:00:00Z");
const node = { node_hash: "node-a", egress_ip: "203.0.113.5", last_egress_update: "v1" };
const source = (ok = true) => ({ ok, status: ok ? "ok" : "error", expires_at: new Date(now + 1000).toISOString(), data: { large: "unused" } });
const result = (sources = [source()]) => ({ egress_ip: node.egress_ip, sources, summary: { ip_type: "unknown", risk_level: "unknown" } });
const compact = (value) => compactBatchResponse({ items: [{ node_hash: node.node_hash, ...value }], total: 1 }, [node], now)[node.node_hash];

test("badge facts preserve source failures and unknown without retaining source payloads", () => {
  assert.deepEqual(compact({ result: result() }), { kind: "cached", sourceCount: 1, expiresAt: now + 1000, ipType: "unknown" });
  assert.equal(compact({ result: result([source(), source(false)]) }).kind, "partial");
  assert.equal(compact({ result: result([source(false)]) }).kind, "sources_failed");
  assert.equal(compact({ result: result([]) }).kind, "empty");
  assert.equal(JSON.stringify(compact({ result: result() })).includes("unused"), false);
});

test("expired, malformed and mismatched observations cannot retain facts", () => {
  for (const expires_at of [new Date(now).toISOString(), "invalid"]) {
    assert.deepEqual(compact({ result: result([{ ...source(), expires_at }]) }), { kind: "expired", sourceCount: 0, expiresAt: null, ipType: "unknown" });
  }
  assert.equal(compact({ result: { ...result(), egress_ip: "203.0.113.6" } }).kind, "mismatch");
  assert.equal(compact({ result: { ...result(), sources: [{}] } }).kind, "failed");
  assert.equal(compact({ result: null }).kind, "failed");
  for (const [code, kind] of [["NOT_FOUND", "empty"], ["CONFLICT", "conflict"], ["RESULT_TOO_LARGE", "too_large"], ["INTERNAL", "failed"]]) {
    assert.equal(compact({ error: { code, message: "untrusted text" } }).kind, kind);
  }
});

test("batch envelope rejects missing, duplicate, unsolicited and ambiguous items", () => {
  const item = { node_hash: node.node_hash, result: result() };
  for (const value of [null, { total: 1, items: [] }, { total: 1, items: [{ ...item, node_hash: "other" }] }, { total: 1, items: [{ ...item, error: {} }] }]) {
    assert.throws(() => compactBatchResponse(value, [node], now));
  }
  assert.throws(() => compactBatchResponse({ total: 2, items: [item, item] }, [node, { ...node, node_hash: "b" }], now));
});

test("expiry timer handles render/effect crossings and timer overflow", () => {
  const entries = { a: compact({ result: result() }) };
  assert.equal(nextBatchExpiryDelay(entries, now, now + 1001), 0);
  assert.equal(nextBatchExpiryDelay(entries, now + 1000, now + 1000), null);
  assert.equal(nextBatchExpiryDelay({ a: { expiresAt: now + 30 * 86400000 } }, now, now), 2147483647);
});

const nodes = Array.from({ length: 101 }, (_, index) => ({ ...node, node_hash: String(index) }));
const emptyResponse = hashes => ({ total: hashes.length, items: hashes.map(node_hash => ({ node_hash, error: { code: "NOT_FOUND" } })) });

test("deduplicated reads are sequential, at most 50 per request and spaced below the route budget", async () => {
  const calls = [], batches = [];
  await readIPQualityBatches([...nodes, nodes[0]], new AbortController().signal, async hashes => {
    calls.push({ hashes, at: Date.now() });
    return emptyResponse(hashes);
  }, entries => batches.push(entries));
  assert.deepEqual(calls.map(call => call.hashes.length), [50, 50, 1]);
  assert.deepEqual(calls.flatMap(call => call.hashes), nodes.map(node => node.node_hash));
  assert.ok(calls[1].at - calls[0].at >= 550);
  assert.ok(calls[2].at - calls[1].at >= 550);
  assert.equal(batches.reduce((sum, batch) => sum + Object.keys(batch).length, 0), 101);
});

test("cancel during spacing prevents every remaining request", async () => {
  const controller = new AbortController();
  let calls = 0;
  await assert.rejects(readIPQualityBatches(nodes, controller.signal, async hashes => { calls++; return emptyResponse(hashes); }, () => setTimeout(() => controller.abort(), 10)), { name: "AbortError" });
  assert.equal(calls, 1);
});

test("late responses after cancellation are discarded and transport failures stop the batch", async () => {
  const controller = new AbortController();
  let batches = 0;
  await assert.rejects(readIPQualityBatches(nodes, controller.signal, async hashes => { controller.abort(); return emptyResponse(hashes); }, () => batches++), { name: "AbortError" });
  assert.equal(batches, 0);
  let calls = 0;
  await assert.rejects(readIPQualityBatches(nodes, new AbortController().signal, async () => { calls++; throw new Error("fixture failure"); }, () => batches++), /fixture failure/);
  assert.equal(calls, 1);
  assert.equal(batches, 0);
});

import assert from "node:assert/strict";
import test from "node:test";
import { normalizeBatchHashes, parseBatchTask, isTerminal, batchErrorKey } from "../src/features/ipQualityBatch/model.ts";

const id = "a".repeat(32), firstHash = "1".repeat(32), secondHash = "2".repeat(32);
const created = "2026-10-14T12:00:00Z", started = "2026-10-14T12:00:01Z", finished = "2026-10-14T12:00:02Z";
const observed = "2026-10-13T12:00:00Z", expires = "2026-10-13T12:01:00Z";
const envelope = { request_id: "batch_1", node_hashes: [firstHash], sources: ["db-ip"], force: false };
const source = (change = {}) => ({ source: "db-ip", kind: "by_ip", ok: true, status: "ok", checked_at: observed, expires_at: expires, ...change });
const completedItem = (change = {}) => ({ node_hash: firstHash, state: "completed", started_at: started, finished_at: finished,
  egress_ip: "8.8.8.8", checked_at: observed, observation_status: "all_ok", sources: [source()], ...change });
function task(items = [completedItem()], change = {}) {
  const progress = { total: items.length, queued: 0, running: 0, completed: 0, failed: 0, canceled: 0 };
  for (const item of items) progress[item.state]++;
  return { id, request_id: "batch_1", state: "completed", sources: ["db-ip"], force: false, created_at: created,
    finished_at: finished, progress, items, ...change };
}
const queuedItem = (node_hash = firstHash) => ({ node_hash, state: "queued" });
const runningItem = (node_hash = firstHash) => ({ node_hash, state: "running", started_at: started });
const failedItem = (node_hash = firstHash) => ({ node_hash, state: "failed", started_at: started, finished_at: finished, error: { code: "NOT_FOUND", message: "node not found" } });
const canceledItem = (node_hash = firstHash) => ({ node_hash, state: "canceled", finished_at: finished });

test("batch task scope is explicit, strictly hexadecimal and limited before deduplication", () => {
  assert.deepEqual(normalizeBatchHashes(`  ${"A".repeat(32)}\r\n\n${firstHash}\r${"a".repeat(32)}  `), ["a".repeat(32), firstHash]);
  assert.deepEqual(normalizeBatchHashes(Array(50).fill(firstHash).join("\n")), [firstHash]);
  assert.equal(normalizeBatchHashes(Array(51).fill(firstHash).join("\n")), null);
  for (const input of ["", " \n ", "g".repeat(32), "a".repeat(31), "a".repeat(33), `${firstHash},${secondHash}`, `${firstHash}\nhttps://example.com`, `${firstHash}\n${secondHash}/`, " ".repeat(17000), null]) {
    assert.equal(normalizeBatchHashes(input), null, String(input).slice(0, 100));
  }
});

test("batch task ownership binds request, task ID, full node/source sets and force", () => {
  assert.ok(parseBatchTask(task(), envelope, id));
  assert.ok(parseBatchTask(task(), undefined, id.toUpperCase()));
  for (const change of [{ request_id: "other" }, { force: true }, { node_hashes: [secondHash] }, { sources: ["egress-consistency"] }, { sources: ["db-ip", "db-ip"] }, { node_hashes: [] }, { request_id: "bad/id" }]) {
    assert.equal(parseBatchTask(task(), { ...envelope, ...change }), null);
  }
  for (const taskID of ["b".repeat(32), `${id}/actions/cancel`, "", null]) assert.equal(parseBatchTask(task(), undefined, taskID), null);
  const two = task([completedItem(), completedItem({ node_hash: secondHash })]);
  assert.ok(parseBatchTask(two, { ...envelope, node_hashes: [secondHash.toUpperCase(), firstHash, firstHash] }));
  assert.equal(parseBatchTask(two, envelope), null);
  assert.equal(parseBatchTask(task([completedItem(), completedItem()])), null);
});

test("known task recovery validates itself without needing a remembered submission", () => {
  const value = task([failedItem()], { request_id: "other-client", force: true });
  const recovered = parseBatchTask(value, undefined, id);
  assert.ok(recovered);
  assert.equal(recovered.request_id, "other-client");
  assert.equal(recovered.progress.failed, 1);
  assert.equal(isTerminal(recovered), true);
  for (const malformed of [null, [], {}, { ...value, items: [] }, { ...value, request_id: {} }, { ...value, state: { toString: null } }, { ...value, force: "false" }, { ...value, id: "bad" }, { ...value, sources: [] }, { ...value, sources: Array(1) }]) assert.equal(parseBatchTask(malformed), null);
});

test("completed is a terminal lifecycle state even when every source failed or was unavailable", () => {
  for (const [status, observation_status] of [["error", "all_failed"], ["unavailable", "unavailable"]]) {
    const parsed = parseBatchTask(task([completedItem({ sources: [source({ ok: false, status, error: "fixture failed" })], observation_status })]));
    assert.ok(parsed);
    assert.equal(parsed.progress.completed, 1);
    assert.equal(parsed.progress.failed, 0);
    assert.equal(parsed.items[0].sources[0].ok, false);
    assert.equal(isTerminal(parsed), true);
  }
  const partial = task([completedItem({ sources: [source(), source({ source: "ipdata", ok: false, status: "unavailable" })], observation_status: "partial_failure" })], { sources: ["db-ip", "ipdata"] });
  assert.ok(parseBatchTask(partial));
  assert.ok(parseBatchTask(task([completedItem({ sources: [], observation_status: "no_observations", checked_at: "0001-01-01T00:00:00Z" })])));
  assert.equal(parseBatchTask(task([completedItem({ sources: [], observation_status: "all_ok" })])), null);
});

test("source evidence cannot contradict its aggregate status or pretend a failure is success", () => {
  for (const change of [{ ok: false }, { status: "error" }, { status: {} }, { ok: "true" }, { source: "future" }, { kind: "via_node" }, { error: { toString: null } }, { checked_at: "bad" }, { expires_at: observed }]) {
    assert.equal(parseBatchTask(task([completedItem({ sources: [source(change)] })])), null);
  }
  for (const observation_status of ["partial_failure", "all_failed", "unavailable", "no_observations", "future", null]) assert.equal(parseBatchTask(task([completedItem({ observation_status })])), null);
  assert.equal(parseBatchTask(task([completedItem({ sources: [source(), source()] })])), null);
  assert.equal(parseBatchTask(task([completedItem({ sources: [source({ source: "ipdata" })] })])), null);
  assert.equal(parseBatchTask(task([completedItem()], { sources: ["db-ip", "egress-consistency"] })), null);
  assert.equal(parseBatchTask(task([completedItem({ checked_at: created })])), null);
});

test("progress is independently recounted and cannot exceed the worker or task limits", () => {
  for (const change of [{ total: 2 }, { completed: 0 }, { failed: 1 }, { queued: -1 }, { running: 0.5 }, { completed: "1" }, { total: Infinity }, { total: NaN }]) {
    const value = task();
    assert.equal(parseBatchTask({ ...value, progress: { ...value.progress, ...change } }), null);
  }
  const items = [firstHash, secondHash, "3".repeat(32)].map(runningItem);
  assert.equal(parseBatchTask(task(items, { state: "running", finished_at: undefined })), null);
  const fifty = Array.from({ length: 50 }, (_, index) => queuedItem(index.toString(16).padStart(32, "0")));
  assert.ok(parseBatchTask(task(fifty, { state: "queued", finished_at: undefined })));
  assert.equal(parseBatchTask(task([...fifty, queuedItem("f".repeat(32))], { state: "queued", finished_at: undefined })), null);
});

test("active task snapshots permit the between-items gap but never invent terminal completion", () => {
  assert.ok(parseBatchTask(task([queuedItem()], { state: "queued", finished_at: undefined })));
  assert.ok(parseBatchTask(task([runningItem(), queuedItem(secondHash)], { state: "running", finished_at: undefined })));
  // The worker records one completion, unlocks, then claims the next item.
  const gap = parseBatchTask(task([completedItem(), queuedItem(secondHash)], { state: "running", finished_at: undefined }));
  assert.ok(gap);
  assert.equal(gap.progress.running, 0);
  assert.equal(isTerminal(gap), false);
  for (const value of [task([queuedItem()]), task([runningItem()]), task([completedItem()], { state: "running", finished_at: undefined }), task([queuedItem()], { state: "running", finished_at: undefined }), task([completedItem()], { finished_at: undefined })]) assert.equal(parseBatchTask(value), null);
});

test("canceling requires pending running work; terminal cancellation preserves completed observations", () => {
  const canceling = parseBatchTask(task([runningItem(), canceledItem(secondHash)], { state: "canceling", cancel_reason: "requested", finished_at: undefined }));
  assert.ok(canceling);
  assert.equal(isTerminal(canceling), false);
  const canceled = parseBatchTask(task([completedItem(), canceledItem(secondHash)], { state: "canceled", cancel_reason: "deadline" }));
  assert.ok(canceled);
  assert.equal(canceled.items[0].observation_status, "all_ok");
  assert.equal(canceled.progress.completed, 1);
  assert.equal(canceled.progress.canceled, 1);
  assert.equal(isTerminal(canceled), true);
  for (const value of [
    task([canceledItem()], { state: "canceled" }), task([canceledItem()], { state: "completed", cancel_reason: "requested" }),
    task([canceledItem()], { state: "canceling", cancel_reason: "requested", finished_at: undefined }),
    task([runningItem(), queuedItem(secondHash)], { state: "canceling", cancel_reason: "requested", finished_at: undefined }),
    task([canceledItem()], { state: "canceled", cancel_reason: "future" }), task([completedItem()], { state: "canceled", cancel_reason: "requested" }),
  ]) assert.equal(parseBatchTask(value), null);
});

test("only completed items may contain observations and failed items require public errors", () => {
  assert.ok(parseBatchTask(task([failedItem()])));
  for (const item of [queuedItem(), runningItem(), failedItem(), canceledItem()]) {
    const state = item.state === "queued" ? "queued" : item.state === "running" ? "running" : item.state === "canceled" ? "canceled" : "completed";
    const extra = { state, finished_at: state === "queued" || state === "running" ? undefined : finished, ...(state === "canceled" ? { cancel_reason: "requested" } : {}) };
    for (const change of [{ egress_ip: "8.8.8.8" }, { observation_status: "all_ok" }, { checked_at: observed }, { sources: [source()] }]) assert.equal(parseBatchTask(task([{ ...item, ...change }], extra)), null);
  }
  for (const error of [undefined, null, { code: "PRIVATE_ERROR", message: "private" }, { code: "INTERNAL", message: {} }, { code: "INTERNAL", message: "" }]) assert.equal(parseBatchTask(task([{ ...failedItem(), error }])), null);
  assert.equal(parseBatchTask(task([completedItem({ error: { code: "INTERNAL", message: "contradiction" } })])), null);
});

test("strict timestamps reject normalization traps while expired cache observations remain readable", () => {
  assert.ok(parseBatchTask(task(), envelope)); // Observation expired the day before this task.
  for (const created_at of ["2026-02-30T00:00:00Z", "2025-02-29T00:00:00Z", "2026-10-14", "2026-10-14T24:00:00Z", "2026-10-14T00:00:00+24:00", "invalid", 0, null]) assert.equal(parseBatchTask({ ...task(), created_at }), null);
  assert.equal(parseBatchTask(task([completedItem({ started_at: observed })])), null);
  assert.equal(parseBatchTask(task([completedItem({ finished_at: created })])), null);
  assert.equal(parseBatchTask(task([completedItem()], { finished_at: started })), null);
  const nano = source({ checked_at: "2026-10-13T12:00:00.123456789+08:00", expires_at: "2026-10-13T12:00:01.123456789+08:00" });
  assert.ok(parseBatchTask(task([completedItem({ sources: [nano], checked_at: nano.checked_at })])));
});

test("observed egress must be an IP literal; mapped and expanded addresses normalize without DNS", () => {
  for (const egress_ip of ["", "example.com", "https://8.8.8.8", "8.8.8.8/32", "8.8.8.8:443", "[::1]", "fe80::1%eth0", "8.008.8.8", {}, null]) assert.equal(parseBatchTask(task([completedItem({ egress_ip })])), null);
  assert.equal(parseBatchTask(task([completedItem({ egress_ip: "::ffff:8.8.8.8" })])).items[0].egress_ip, "8.8.8.8");
  assert.equal(parseBatchTask(task([completedItem({ egress_ip: "2001:4860:4860:0:0:0:0:8888" })])).items[0].egress_ip, "2001:4860:4860::8888");
});

test("metadata limits count UTF-8 bytes and unknown raw fields are not retained", () => {
  assert.ok(parseBatchTask(task([completedItem({ sources: [source({ error: "😀".repeat(128) })] })], { request_id: "x".repeat(64) })));
  assert.equal(parseBatchTask(task([completedItem({ sources: [source({ error: "😀".repeat(129) })] })])), null);
  assert.equal(parseBatchTask(task([completedItem({ sources: [source({ error: "x".repeat(513) })] })])), null);
  assert.equal(parseBatchTask(task([failedItem()], { request_id: "x".repeat(65) })), null);
  const original = task([completedItem({ sources: [source({ data: { private: true } })], summary: { country: "private" } })], { config_fingerprint: "private" });
  const parsed = parseBatchTask(original);
  assert.ok(parsed);
  assert.equal(JSON.stringify(parsed).includes("private"), false);
  parsed.items[0].sources[0].ok = false;
  parsed.sources[0] = "changed";
  parsed.progress.completed = 0;
  assert.equal(original.items[0].sources[0].ok, true);
  assert.equal(original.sources[0], "db-ip");
  assert.equal(original.progress.completed, 1);
});

test("batch errors preserve uncertainty, limited idempotency and the distinction between observing and canceling", () => {
  assert.match(batchErrorKey(undefined, "submit"), /未能确认.*重试可能再次执行/);
  assert.match(batchErrorKey(503, "submit"), /保留原提交/);
  assert.match(batchErrorKey(404, "poll"), /过期.*淘汰.*不代表从未执行/);
  assert.match(batchErrorKey(undefined, "cancel"), /未获确认.*仍在运行/);
  assert.match(batchErrorKey(undefined, "poll"), /暂停查询.*继续运行/);
  assert.match(batchErrorKey(429), /手动重试/);
  assert.match(batchErrorKey(409, "submit"), /其他范围/);
  assert.equal(batchErrorKey(401), batchErrorKey(403));
});

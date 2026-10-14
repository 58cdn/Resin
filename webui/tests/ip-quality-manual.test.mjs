import assert from "node:assert/strict";
import test from "node:test";
import { CHECK_WAIT_MS, READ_WAIT_MS, normalizeManualIP, normalizeSubject, nodeIdentity, sameNode, sourceCatalog, selectable, validSelection, isManualResult, resultStatus, manualErrorKey } from "../src/features/ipQualityManual/model.ts";

const catalog = [{ name: "db-ip", kind: "by_ip", enabled: true, available: true, requires_key: false }, { name: "egress-consistency", kind: "via_node", enabled: true, available: true, requires_key: false }];
const prepared = { kind: "ip", value: "8.8.8.8", ip: "8.8.8.8", sources: catalog };
const checked_at = "2026-10-14T10:00:00Z", expires_at = "2026-10-14T11:00:00Z";
const fixture = () => ({ egress_ip: "8.8.8.8", checked_at, summary: { country: "unknown", asn: null, fraud_score: 0, egress_consistent: false, services: { chatgpt: "unknown" }, abuse_flags: [] }, sources: [{ source: "db-ip", kind: "by_ip", ok: true, status: "ok", checked_at, expires_at, data: { score: 0, flag: false } }] });

test("manual literal normalization rejects paths, ambiguous IPv4 and incomplete identities", () => {
  for (const value of ["", "sources", "lookup", "8.8.8.8/32", "8.8.8.8:80", "8.008.8.8", "256.1.1.1", "fe80::1%eth0", "[::1]", "https://8.8.8.8"]) assert.equal(normalizeManualIP(value), null, value);
  assert.equal(normalizeManualIP(" 2001:4860:4860:0:0:0:0:8888 "), "2001:4860:4860::8888");
  assert.equal(normalizeManualIP("::ffff:808:808"), "8.8.8.8");
  assert.equal(normalizeSubject("node", "A".repeat(32)), "a".repeat(32));
  assert.equal(normalizeSubject("node", "a".repeat(31)), null);
});
test("selection is explicit and nonempty; keyed, unavailable, disabled, future and via-node IP sources cannot run", () => {
  assert.equal(validSelection(prepared, []), false);
  assert.equal(validSelection(prepared, ["db-ip", "db-ip"]), false);
  assert.equal(validSelection(prepared, ["egress-consistency"]), false);
  assert.equal(validSelection(prepared, ["db-ip"]), true);
  for (const change of [{ name: "future" }, { enabled: false }, { available: false }, { requires_key: true }]) assert.equal(selectable({ ...catalog[0], ...change }, "node"), false);
  assert.equal(validSelection({ ...prepared, kind: "node" }, ["db-ip", "egress-consistency"]), true);
});
test("source catalogs reject duplicates, wrong kinds and malformed availability", () => {
  assert.deepEqual(sourceCatalog({ items: catalog }), catalog);
  for (const items of [[catalog[0], catalog[0]], [{ ...catalog[0], kind: "via_node" }], [{ ...catalog[0], enabled: "true" }], [{ ...catalog[0], kind: { toString: null } }], [null]]) assert.equal(sourceCatalog({ items }), null);
});
test("node snapshot validation and version identity reject A-to-B-to-A stale egress", () => {
  const hash = "a".repeat(32), raw = { node_hash: hash, egress_ip: "::ffff:8.8.8.8", last_egress_update: "1", display_tag: "Fixture" };
  const a = nodeIdentity(raw, hash);
  assert.equal(a.ip, "8.8.8.8");
  assert.equal(sameNode(a, nodeIdentity({ ...raw, last_egress_update: "3" }, hash)), false);
  assert.equal(sameNode(a, nodeIdentity({ ...raw, egress_ip: "9.9.9.9" }, hash)), false);
  assert.equal(nodeIdentity({ ...raw, node_hash: "b".repeat(32) }, hash), null);
  assert.equal(nodeIdentity({ ...raw, egress_ip: "" }, hash), null);
});
test("response guard binds IP and exact requested sources without losing zero, false or unknown", () => {
  const value = fixture();
  assert.equal(isManualResult(value, prepared, ["db-ip"]), true);
  assert.equal(value.summary.fraud_score, 0); assert.equal(value.summary.egress_consistent, false);
  assert.equal(isManualResult({ ...value, egress_ip: "9.9.9.9" }, prepared, ["db-ip"]), false);
  assert.equal(isManualResult(value, prepared, ["db-ip", "ipdata"]), false);
  assert.equal(isManualResult({ ...value, sources: [...value.sources, ...value.sources] }, prepared), false);
  assert.equal(isManualResult({ ...value, sources: [{ ...value.sources[0], kind: "via_node" }] }, prepared), false);
  assert.equal(isManualResult({ ...value, sources: [] }, prepared, ["db-ip"]), false);
});
test("malformed nested summary fields and timestamps cannot reach rendering", () => {
  for (const change of [{ ip_type: { toString: null } }, { services: { chatgpt: {} } }, { egress_consistent: "true" }, { abuse_flags: [null] }, { fraud_score: Infinity }, { asn: NaN }]) assert.equal(isManualResult({ ...fixture(), summary: { ...fixture().summary, ...change } }, prepared), false);
  assert.equal(isManualResult({ ...fixture(), checked_at: "bad" }, prepared), false);
  assert.equal(isManualResult({ ...fixture(), sources: [{ ...fixture().sources[0], expires_at: checked_at }] }, prepared), false);
});
test("source failure, expiry and no evidence remain separate from received observations", () => {
  const value = fixture(), now = Date.parse(checked_at);
  assert.match(resultStatus(value, now), /不代表/);
  assert.match(resultStatus(value, Date.parse(expires_at)), /过期/);
  assert.match(resultStatus({ ...value, sources: [] }, now), /不能判断/);
  const failed = { ...value.sources[0], ok: false, status: "error" };
  assert.match(resultStatus({ ...value, sources: [failed] }, now), /所有来源/);
  assert.match(resultStatus({ ...value, sources: [value.sources[0], failed] }, now), /部分来源失败/);
  assert.match(resultStatus({ ...value, sources: [{ ...failed, status: "unavailable" }] }, now), /不可用/);
});
test("transport errors never prove no execution; rate-limit and cache errors stay explicit", () => {
  assert.match(manualErrorKey(), /无法确认/);
  assert.match(manualErrorKey(429), /手动重试/);
  assert.match(manualErrorKey(503), /繁忙/);
  assert.match(manualErrorKey(409), /不会自动探测/);
  assert.match(manualErrorKey(404, true), /暂无有效缓存/);
  assert.equal(CHECK_WAIT_MS, 130000); assert.equal(READ_WAIT_MS, 20000);
});

import test from "node:test";
import assert from "node:assert/strict";
import { normalizeCacheIP, isIPCacheResult, ipCacheStatus, cacheReadErrorKey } from "../src/features/ipQuality/cache.ts";

const now = Date.parse("2026-10-14T12:00:00Z");
const source = (change = {}) => ({ source: "db-ip", kind: "by_ip", ok: true, status: "ok", checked_at: new Date(now - 1000).toISOString(), expires_at: new Date(now + 1000).toISOString(), data: { flag: false, score: 0 }, ...change });
const result = (sources = [source()]) => ({ egress_ip: "8.8.8.8", checked_at: new Date(now).toISOString(), summary: {}, sources });

test("normalizes strict IPv4, expanded IPv6, and mapped IPv6 without DNS", () => {
  for (const [input, expected] of [[" 8.8.8.8 ", "8.8.8.8"], ["2001:4860:4860:0000:0000:0000:0000:8888", "2001:4860:4860::8888"], ["2606:4700:4700::ABCD", "2606:4700:4700::abcd"], ["::ffff:8.8.8.8", "8.8.8.8"], ["0:0:0:0:0:ffff:0808:0808", "8.8.8.8"]]) assert.equal(normalizeCacheIP(input), expected, input);
});
test("rejects route collisions, non-literals, ambiguous IPv4 and scoped addresses", () => {
  for (const value of ["", "sources", "lookup", "example.com", "https://8.8.8.8", "8.8.8.8:443", "8.8.8.8/32", "[2001:4860::1]", "fe80::1%eth0", "8.8.8", "01.2.3.4", "256.1.1.1", "0x08080808", "1::2::3", "1:2:3:4:5:6:7:8:9", "::ffff:08.8.8.8", "8.8.8.8?x", "8.8.8.8#x", "1.2.3.4@evil"]) assert.equal(normalizeCacheIP(value), null, value);
});
test("literal syntax does not claim public-address policy: the server validates ranges", () => {
  assert.equal(normalizeCacheIP("127.0.0.1"), "127.0.0.1");
  assert.equal(normalizeCacheIP("::1"), "::1");
});
test("checks query ownership and the by-IP response shape before rendering facts", () => {
  assert.equal(isIPCacheResult(result(), "8.8.8.8"), true);
  assert.equal(isIPCacheResult(result(), normalizeCacheIP("::ffff:8.8.8.8")), true);
  for (const value of [null, {}, { items: [] }, { ...result(), egress_ip: "1.1.1.1" }, { ...result(), summary: null }, result([source({ kind: "via_node" })]), result([source(), source()]), result([source({ data: null })]), result([source({ error: {} })])]) assert.equal(isIPCacheResult(value, "8.8.8.8"), false);
});
test("distinguishes valid cache, partial failure, source unavailability, all-failed and empty", () => {
  assert.equal(ipCacheStatus(result(), now), "cached");
  assert.equal(ipCacheStatus(result([source(), source({ source: "ipdata", ok: false, status: "error" })]), now), "partial");
  assert.equal(ipCacheStatus(result([source({ ok: false, status: "unavailable" })]), now), "unavailable");
  assert.equal(ipCacheStatus(result([source({ ok: false, status: "error" })]), now), "failed");
  assert.equal(ipCacheStatus(result([]), now), "empty");
});
test("malformed summary values cannot reach enum coercion or crash rendering", () => {
  for (const summary of [{ ip_type: { toString: null } }, { ip_origin: [] }, { risk_level: {} }, { country: false }, { asn: "123" }, { fraud_score: Infinity }, { abuse_flags: [null] }]) assert.equal(isIPCacheResult({ ...result(), summary }, "8.8.8.8"), false);
  assert.equal(isIPCacheResult({ ...result(), summary: { country: null, asn: null, ip_type: "future", abuse_flags: null } }, "8.8.8.8"), true);
});
test("any expired or unparseable source invalidates the aggregate at the exact boundary", () => {
  assert.equal(ipCacheStatus(result(), now + 999), "cached");
  assert.equal(ipCacheStatus(result(), now + 1000), "expired");
  assert.equal(ipCacheStatus(result([source(), source({ source: "ipdata", expires_at: "invalid" })]), now), "expired");
});
test("validation and classification preserve raw false, zero, errors and unknown", () => {
  const value = result([source({ error: "<b>fixture error</b>" })]);
  value.summary = { fraud_score: 0, risk_level: "unknown", abuse_flags: [], asn: null };
  const before = JSON.stringify(value);
  assert.equal(isIPCacheResult(value, "8.8.8.8"), true);
  ipCacheStatus(value, now);
  assert.equal(JSON.stringify(value), before);
});
test("404 cannot claim never checked; permission, rate limit, unavailable and failure remain distinct", () => {
  assert.match(cacheReadErrorKey(404), /可能尚未检测.*已过期.*变更/);
  for (const status of [400, 401, 403, 429, 503, 500]) assert.notEqual(cacheReadErrorKey(status), cacheReadErrorKey(404));
  assert.match(cacheReadErrorKey(429), /稍后手动重试/);
  assert.equal(cacheReadErrorKey(401), cacheReadErrorKey(403));
});

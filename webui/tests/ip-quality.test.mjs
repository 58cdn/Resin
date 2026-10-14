import assert from "node:assert/strict";
import test from "node:test";
import { cacheMatchesNode, isFreshSource, nextExpiryDelay, qualityValueLabel } from "../src/features/nodes/ip-quality.ts";

test("cache expiry is exclusive and invalid dates are never fresh", () => {
  const expiry = Date.parse("2026-10-14T12:00:00Z");
  assert.equal(isFreshSource({ expires_at: "2026-10-14T12:00:00Z" }, expiry - 1), true);
  assert.equal(isFreshSource({ expires_at: "2026-10-14T12:00:00Z" }, expiry), false);
  assert.equal(isFreshSource({ expires_at: "invalid" }, expiry), false);
  assert.equal(isFreshSource({ expires_at: "" }, expiry), false);
});

test("another egress or a node without egress cannot reuse cached observations", () => {
  const result = { egress_ip: "203.0.113.5" };
  assert.equal(cacheMatchesNode(result, "203.0.113.5"), true);
  assert.equal(cacheMatchesNode(result, "203.0.113.6"), false);
  assert.equal(cacheMatchesNode(result, ""), false);
  assert.equal(cacheMatchesNode({ egress_ip: "" }, ""), false);
});

test("an expiry crossed between render and effect schedules an immediate update", () => {
  const expiry = Date.parse("2026-10-14T12:00:00Z");
  const sources = [{expires_at: new Date(expiry).toISOString()}, {expires_at: new Date(expiry + 60000).toISOString()}];
  assert.equal(nextExpiryDelay(sources, expiry - 10, expiry + 5), 0);
  assert.equal(nextExpiryDelay(sources, expiry + 5, expiry + 5), 59996);
  assert.equal(nextExpiryDelay(sources, expiry + 60000, expiry + 60000), null);
  assert.equal(nextExpiryDelay([{expires_at:"invalid"}], expiry, expiry), null);
  assert.equal(nextExpiryDelay([{expires_at:new Date(expiry + 30 * 86400000).toISOString()}], expiry, expiry), 2147483647);
});

test("unknown and future enum values cannot turn into reassuring labels", () => {
  for (const value of [undefined, "", "unknown", "unavailable", "future_status", "__proto__", "constructor"]) {
    assert.equal(qualityValueLabel(value), "未知");
  }
  assert.equal(qualityValueLabel("region_blocked"), "地区受限");
  assert.equal(qualityValueLabel("challenge"), "发现挑战");
  assert.equal(qualityValueLabel("available"), "可用观测");
});

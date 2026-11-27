import assert from "node:assert/strict";
import test from "node:test";
import { isLowRemainingQuota, quotaDisplayPercentage } from "./quota-display.mjs";
import { providerSpec } from "./providers.mjs";

test("quotaDisplayPercentage matches official remaining percentages for Antigravity and Codex", () => {
  assert.equal(providerSpec("claude")?.quotaDisplay, "used");
  assert.equal(providerSpec("antigravity")?.quotaDisplay, "remaining");
  assert.equal(providerSpec("codex")?.quotaDisplay, "remaining");

  // Antigravity: 8.8% used -> 91.2% remaining; 16% used -> 84% remaining; 0% used -> 100% remaining
  assert.equal(quotaDisplayPercentage(8.8, "remaining"), 91.2);
  assert.equal(quotaDisplayPercentage(16, "remaining"), 84);
  assert.equal(quotaDisplayPercentage(0, "remaining"), 100);

  // Codex: 7% used -> 93% remaining; 100% used -> 0% remaining
  assert.equal(quotaDisplayPercentage(7, "remaining"), 93);
  assert.equal(quotaDisplayPercentage(100, "remaining"), 0);

  // Claude keeps used percentage
  assert.equal(quotaDisplayPercentage(28, "used"), 28);
  assert.equal(quotaDisplayPercentage(8.8, "used"), 8.8);
  assert.equal(quotaDisplayPercentage(undefined, "remaining"), null);
  assert.equal(quotaDisplayPercentage(NaN, "used"), null);
});

test("isLowRemainingQuota flags <= 20% remaining only in remaining mode", () => {
  assert.equal(isLowRemainingQuota(8.8, "remaining"), false);
  assert.equal(isLowRemainingQuota(79.9, "remaining"), false);
  assert.equal(isLowRemainingQuota(80, "remaining"), true);
  assert.equal(isLowRemainingQuota(100, "remaining"), true);
  assert.equal(isLowRemainingQuota(95, "used"), false);
});

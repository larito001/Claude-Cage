export function quotaDisplayPercentage(usedPercentage, mode = "used") {
  if (!Number.isFinite(usedPercentage)) return null;
  const clampedUsed = Math.max(0, Math.min(100, usedPercentage));
  const raw = mode === "remaining" ? 100 - clampedUsed : clampedUsed;
  return Math.round(raw * 10) / 10;
}

export function isLowRemainingQuota(usedPercentage, mode = "used", threshold = 20) {
  if (mode !== "remaining") return false;
  const remaining = quotaDisplayPercentage(usedPercentage, "remaining");
  return remaining !== null && remaining <= threshold;
}

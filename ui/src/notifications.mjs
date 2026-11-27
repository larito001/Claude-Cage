export const notificationModes = Object.freeze(["unfocused", "always", "off"]);

export const defaultNotificationProviders = Object.freeze({
  claude: true,
  antigravity: true,
  codex: true,
});

// Off until the user turns notifications on in Settings; enabling them is
// what asks the system for permission.
export const defaultNotificationPreferences = Object.freeze({
  turn_completion: "off",
  approval: "off",
  sound: true,
  providers: defaultNotificationProviders,
});

export function normalizeNotificationPreferences(raw) {
  const source = raw && typeof raw === "object" ? raw : {};
  const rawProviders = source.providers && typeof source.providers === "object" ? source.providers : {};
  return {
    turn_completion: notificationModes.includes(source.turn_completion)
      ? source.turn_completion
      : defaultNotificationPreferences.turn_completion,
    approval: notificationModes.includes(source.approval)
      ? source.approval
      : defaultNotificationPreferences.approval,
    sound: typeof source.sound === "boolean" ? source.sound : defaultNotificationPreferences.sound,
    providers: {
      claude: typeof rawProviders.claude === "boolean" ? rawProviders.claude : defaultNotificationProviders.claude,
      antigravity:
        typeof rawProviders.antigravity === "boolean"
          ? rawProviders.antigravity
          : defaultNotificationProviders.antigravity,
      codex: typeof rawProviders.codex === "boolean" ? rawProviders.codex : defaultNotificationProviders.codex,
    },
  };
}

export function notificationModeForEvent(prefs, kind) {
  const normalized = normalizeNotificationPreferences(prefs);
  if (kind === "turn_completed") return normalized.turn_completion;
  if (kind === "approval_requested") return normalized.approval;
  return "off";
}

export function shouldDeliverNotification(prefs, event, viewState = {}) {
  if (!event || typeof event !== "object" || !event.kind) return false;
  const normalized = normalizeNotificationPreferences(prefs);
  const providerKey =
    event.provider === "antigravity" || event.provider === "codex" ? event.provider : "claude";
  if (normalized.providers[providerKey] === false) return false;
  const mode = notificationModeForEvent(normalized, event.kind);
  if (mode === "off") return false;
  if (mode === "always") return true;
  // "unfocused": suppress only when SuperCage is focused AND the user is already viewing this terminal.
  return !(Boolean(viewState.windowFocused) && Boolean(viewState.viewingTerminal));
}

export function buildNotificationContent(event, tr = (text, ...values) =>
  values.length ? text.replace(/\{(\d+)\}/g, (match, index) => (Number(index) < values.length ? String(values[index]) : match)) : text,
) {
  const parts = [event?.workspace_name, event?.terminal_title].filter((item) => typeof item === "string" && item.trim());
  const label = parts.join(" · ");
  const provider = event?.provider;
  if (event?.kind === "approval_requested") {
    const title =
      provider === "antigravity"
        ? tr("Antigravity 等待确认")
        : provider === "codex"
          ? tr("Codex 等待确认")
          : tr("Claude Code 等待确认");
    return {
      title,
      body: label ? tr("{0} · 点击前往终端处理", label) : tr("终端有操作等待确认，点击返回终端"),
    };
  }
  const title =
    provider === "antigravity"
      ? tr("Antigravity 已完成回复")
      : provider === "codex"
        ? tr("Codex 已完成回复")
        : tr("Claude Code 已完成回复");
  return {
    title,
    body: label ? tr("{0} · 点击返回终端", label) : tr("本轮回复已结束，点击返回终端"),
  };
}

export function createNotificationDeduplicator(limit = 256) {
  const seen = new Set();
  const order = [];
  return {
    admit(id) {
      if (typeof id !== "string" || !id) return true;
      if (seen.has(id)) return false;
      seen.add(id);
      order.push(id);
      if (order.length > limit) {
        seen.delete(order.shift());
      }
      return true;
    },
  };
}

export async function dispatchDesktopNotification(
  event,
  { prefs, viewState, nativeAvailable, native, tr, connectionId = "", dedup, webNotification = globalThis.Notification, onFocus } = {},
) {
  if (!event || !shouldDeliverNotification(prefs, event, viewState)) {
    return { delivered: false, reason: "suppressed" };
  }
  if (dedup && !dedup.admit(event.id)) {
    return { delivered: false, reason: "duplicate" };
  }
  const normalized = normalizeNotificationPreferences(prefs);
  const { title, body } = buildNotificationContent(event, tr);
  if (nativeAvailable?.()) {
    return native("showNotification", {
      id: event.id || "",
      title,
      body,
      terminal_id: event.terminal_id || "",
      connection_id: connectionId || "",
      sound: normalized.sound,
    });
  }
  if (typeof webNotification === "function" && webNotification.permission === "granted") {
    const notice = new webNotification(title, {
      body,
      tag: event.id || undefined,
      silent: !normalized.sound,
    });
    if (onFocus) {
      notice.onclick = () => {
        globalThis.window?.focus?.();
        onFocus({ terminal_id: event.terminal_id || "", connection_id: connectionId || "" });
        notice.close?.();
      };
    }
    return { delivered: true, permission: "authorized" };
  }
  return { delivered: false, reason: "unavailable" };
}

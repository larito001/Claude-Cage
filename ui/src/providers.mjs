export const PROVIDERS = Object.freeze([
  Object.freeze({
    id: "claude",
    name: "Claude",
    cliName: "Claude Code",
    cliCommand: "claude",
    terminalKind: "claude",
    loginKind: "claude-login",
    desktopLabel: "Claude Desktop",
    browserSpaceLabel: "Claude 浏览器",
    spaceBadge: "Claude 空间",
    shortBadge: "Claude",
    homeURL: "https://claude.ai/",
    supportsAutoHandoff: true,
    supportsDeviceAuth: false,
    supportsQuota: true,
    quotaDisplay: "used",
  }),
  Object.freeze({
    id: "antigravity",
    name: "Antigravity",
    cliName: "Antigravity CLI",
    cliCommand: "agy",
    terminalKind: "antigravity",
    loginKind: "antigravity-login",
    desktopLabel: "Antigravity Desktop",
    browserSpaceLabel: "Antigravity 浏览器",
    spaceBadge: "Antigravity 空间",
    shortBadge: "AGY",
    homeURL: "https://antigravity.google/",
    supportsAutoHandoff: false,
    supportsDeviceAuth: false,
    supportsQuota: true,
    quotaDisplay: "remaining",
  }),
  Object.freeze({
    id: "codex",
    name: "Codex",
    cliName: "Codex CLI",
    cliCommand: "codex",
    terminalKind: "codex",
    loginKind: "codex-login",
    desktopLabel: "Codex Desktop",
    browserSpaceLabel: "Codex 浏览器",
    spaceBadge: "Codex 空间",
    shortBadge: "Codex",
    homeURL: "https://chatgpt.com/",
    supportsAutoHandoff: false,
    supportsDeviceAuth: true,
    supportsQuota: true,
    quotaDisplay: "remaining",
  }),
]);

const BY_ID = new Map(PROVIDERS.map((item) => [item.id, item]));
const BY_TERMINAL_KIND = new Map();
for (const item of PROVIDERS) {
  BY_TERMINAL_KIND.set(item.terminalKind, item);
  BY_TERMINAL_KIND.set(item.loginKind, item);
}

export function providerSpec(id) {
  return BY_ID.get(String(id || "").trim().toLowerCase()) || null;
}

export function validBrowserSpace(space) {
  return BY_ID.has(String(space || "").trim().toLowerCase());
}

export function browserSpaceSpec(space) {
  return providerSpec(space);
}

export function terminalProvider(kind) {
  return BY_TERMINAL_KIND.get(String(kind || "").trim().toLowerCase()) || null;
}

export function isProviderLoginTerminal(kind) {
  const normalized = String(kind || "").trim().toLowerCase();
  return PROVIDERS.some((item) => item.loginKind === normalized);
}

export function isAgentTerminalTab(tab) {
  if (!tab) return false;
  if (tab.cli_session_id || tab.claude_account_id || tab.provider_account_id) return true;
  return Boolean(terminalProvider(tab.terminalKind));
}

export function tabProviderId(tab) {
  if (!tab) return "";
  const explicit = String(tab.provider || "").trim().toLowerCase();
  if (BY_ID.has(explicit)) return explicit;
  const byKind = terminalProvider(tab.terminalKind);
  if (byKind) return byKind.id;
  if (tab.cli_session_id || tab.claude_account_id || tab.historySession) return "claude";
  return "";
}

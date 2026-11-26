// Match printed authorization links in xterm's buffer across Claude, Antigravity, and Codex.
export const claudeLoginLink = /https:\/\/(?:claude\.ai|claude\.com|console\.anthropic\.com|platform\.claude\.com|platform\.anthropic\.com)\/(?:cai\/)?oauth\/authorize\/?\?[^\s"'<>\\\x1b]+/i;

export const terminalLoginLink = /https:\/\/(?:(?:claude\.ai|claude\.com|console\.anthropic\.com|platform\.claude\.com|platform\.anthropic\.com)\/(?:cai\/)?oauth\/authorize\/?\?[^\s"'<>\\\x1b]+|accounts\.google\.com\/(?:o\/oauth2(?:\/[A-Za-z0-9._-]+)*|AccountChooser(?:\/[A-Za-z0-9._-]+)*)\/?\?[^\s"'<>\\\x1b]+|antigravity\.google(?:\.com)?\/(?:login|auth|oauth-callback|oauth(?:\/[A-Za-z0-9._-]+)*)\/?\?[^\s"'<>\\\x1b]+|(?:auth|auth0)\.openai\.com\/(?:(?:oauth\/authorize|authorize|u\/login(?:\/[A-Za-z0-9._-]+)*)\/?\?[^\s"'<>\\\x1b]+|(?:codex\/device|codex\/[A-Za-z0-9._/-]+|device|activate)(?:\/?(?:\?[^\s"'<>\\\x1b]+)?)?)|(?:chatgpt\.com|chat\.openai\.com)\/(?:activate|codex\/device|auth\/[A-Za-z0-9._/-]+)(?:\/?(?:\?[^\s"'<>\\\x1b]+)?)?)/i;

const urlFragmentPattern = /^[A-Za-z0-9\-._~:/?#[\]@!$&'()*+,;=%]+$/;
const urlStartInLine = /(https:\/\/[A-Za-z0-9\-._~:/?#[\]@!$&'()*+,;=%]+)\s*$/;

export function isClaudeLoginLink(uri) {
  if (typeof uri !== "string" || uri.length > 8192) return false;
  const match = claudeLoginLink.exec(uri);
  return match?.[0] === uri;
}

export function isTerminalLoginLink(uri) {
  if (typeof uri !== "string" || uri.length > 8192) return false;
  const match = terminalLoginLink.exec(uri);
  return match?.[0] === uri;
}

export function normalizeTerminalCopyText(text, options = undefined) {
  if (typeof text !== "string" || !text.includes("\n")) return text;
  const rawSplit = text.split(/\r?\n/);
  while (rawSplit.length && !rawSplit[0].trim()) rawSplit.shift();
  while (rawSplit.length && !rawSplit[rawSplit.length - 1].trim()) rawSplit.pop();
  if (rawSplit.length < 2) return text;
  // An interior blank line always separates distinct paragraphs/commands.
  if (rawSplit.some(line => !line.trim())) return text;
  const rawLines = rawSplit.map(line => line.trim());
  if (!/^https?:\/\//i.test(rawLines[0])) return text;
  if (!rawLines.every(line => urlFragmentPattern.test(line))) return text;
  if (rawLines.slice(1).some(line => /^https?:\/\//i.test(line))) return text;
  const joined = rawLines.join("");
  if (isTerminalLoginLink(joined) || options?.wrapped === true) return joined;
  const cols = Number(options?.cols);
  const minWrapWidth = Number.isFinite(cols) && cols > 24 ? Math.max(68, cols - 12) : 68;
  if (rawLines[0].includes("?") && rawLines.slice(0, -1).every(line => line.length >= minWrapWidth)) {
    return joined;
  }
  return text;
}

export function extractTerminalLoginLink(lines) {
  if (!Array.isArray(lines) || !lines.length) return "";
  let found = "";
  for (let i = 0; i < lines.length; i++) {
    const line = String(lines[i] || "");
    const direct = line.match(new RegExp(terminalLoginLink.source, "gi"));
    if (direct?.length) {
      const candidate = direct[direct.length - 1];
      if (isTerminalLoginLink(candidate)) found = candidate;
    }
    const startMatch = urlStartInLine.exec(line);
    if (!startMatch) continue;
    let combined = startMatch[1];
    for (let j = i + 1; j < lines.length && combined.length <= 8192; j++) {
      const next = String(lines[j] || "").trim();
      if (!next || /^https?:\/\//i.test(next) || !urlFragmentPattern.test(next)) break;
      combined += next;
      const candidateMatch = terminalLoginLink.exec(combined);
      if (candidateMatch?.[0] === combined) found = combined;
    }
    const finalMatch = terminalLoginLink.exec(combined);
    if (finalMatch?.[0] === combined) found = combined;
  }
  return found;
}

import test from "node:test";
import assert from "node:assert/strict";
import {
  isClaudeLoginLink,
  isTerminalLoginLink,
  normalizeTerminalCopyText,
  extractTerminalLoginLink,
} from "./terminal-login-link.mjs";

test("isTerminalLoginLink accepts Claude, Antigravity, and Codex authorization links", () => {
  const valid = [
    "https://claude.ai/oauth/authorize?state=abc",
    "https://claude.com/cai/oauth/authorize?state=abc",
    "https://accounts.google.com/o/oauth2/auth?access_type=offline&client_id=1071006060591.apps.googleusercontent.com&redirect_uri=https%3A%2F%2Fantigravity.google%2Foauth-callback&response_type=code&state=xyz",
    "https://accounts.google.com/o/oauth2/v2/auth?client_id=test&state=123",
    "https://antigravity.google/oauth/authorize?state=1",
    "https://auth.openai.com/oauth/authorize?client_id=app_123&response_type=code",
    "https://auth.openai.com/codex/device",
    "https://chatgpt.com/activate",
  ];
  for (const url of valid) {
    assert.equal(isTerminalLoginLink(url), true, `expected valid: ${url}`);
  }
  assert.equal(isClaudeLoginLink(valid[0]), true);
  assert.equal(isClaudeLoginLink(valid[2]), false);

  const invalid = [
    "https://example.com/oauth/authorize?state=1",
    "http://accounts.google.com/o/oauth2/auth?state=1",
    "https://accounts.google.com/o/oauth2/auth",
    "https://chatgpt.com/c/12345",
  ];
  for (const url of invalid) {
    assert.equal(isTerminalLoginLink(url), false, `expected invalid: ${url}`);
  }
});

test("normalizeTerminalCopyText unwraps multi-line TUI URLs while preserving normal selections", () => {
  assert.equal(normalizeTerminalCopyText("CopyMe"), "CopyMe");
  assert.equal(normalizeTerminalCopyText("echo one\necho two"), "echo one\necho two");
  assert.equal(
    normalizeTerminalCopyText("https://claude.ai/oauth/authorize?a=1\nhttps://claude.ai/oauth/authorize?b=2"),
    "https://claude.ai/oauth/authorize?a=1\nhttps://claude.ai/oauth/authorize?b=2",
  );
  assert.equal(
    normalizeTerminalCopyText("https://example.com\nREADME.md\nmain.go"),
    "https://example.com\nREADME.md\nmain.go",
  );
  assert.equal(
    normalizeTerminalCopyText("https://example.com/api?foo=bar\n\nsecond_line"),
    "https://example.com/api?foo=bar\n\nsecond_line",
  );
  const wrapped = [
    " https://accounts.google.com/o/oauth2/auth?access_type=offline&client_id=107100",
    " 6060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com&code_chall",
    " enge=s9o1zzIhOkEa2ae9H8pbSn1&redirect_uri=https%3A%2F%2Fantigravity.google%2Foauth-callback",
  ].join("\n");
  assert.equal(
    normalizeTerminalCopyText(wrapped),
    "https://accounts.google.com/o/oauth2/auth?access_type=offline&client_id=1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com&code_challenge=s9o1zzIhOkEa2ae9H8pbSn1&redirect_uri=https%3A%2F%2Fantigravity.google%2Foauth-callback",
  );
});

test("extractTerminalLoginLink reconstructs wrapped Antigravity TUI OAuth URLs", () => {
  const lines = [
    " Open the URL below in your browser:",
    " ──────────────────────────────────────────────────────────────────────────────",
    " https://accounts.google.com/o/oauth2/auth?access_type=offline&client_id=107100",
    " 6060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com&code_chall",
    " enge=abc&redirect_uri=https%3A%2F%2Fantigravity.google%2Foauth-callback&state=xyz",
    " ──────────────────────────────────────────────────────────────────────────────",
    "  (1–20 of 26 lines)",
  ];
  assert.equal(
    extractTerminalLoginLink(lines),
    "https://accounts.google.com/o/oauth2/auth?access_type=offline&client_id=1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com&code_challenge=abc&redirect_uri=https%3A%2F%2Fantigravity.google%2Foauth-callback&state=xyz",
  );
});

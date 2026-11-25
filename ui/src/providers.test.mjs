import assert from "node:assert/strict";
import test from "node:test";
import {
  PROVIDERS,
  browserSpaceSpec,
  isAgentTerminalTab,
  isProviderLoginTerminal,
  providerSpec,
  terminalProvider,
  validBrowserSpace,
} from "./providers.mjs";

test("provider registry exposes claude, antigravity, and codex with isolated spaces and terminals", () => {
  assert.deepEqual(
    PROVIDERS.map((item) => item.id),
    ["claude", "antigravity", "codex"],
  );
  assert.equal(providerSpec("antigravity")?.cliCommand, "agy");
  assert.equal(providerSpec("codex")?.supportsDeviceAuth, true);
  assert.equal(providerSpec("claude")?.supportsAutoHandoff, true);
  assert.equal(providerSpec("unknown"), null);

  for (const id of ["claude", "antigravity", "codex"]) {
    assert.equal(validBrowserSpace(id), true);
    assert.equal(browserSpaceSpec(id)?.id, id);
  }
  assert.equal(validBrowserSpace("general"), false);

  assert.equal(terminalProvider("antigravity")?.id, "antigravity");
  assert.equal(terminalProvider("codex-login")?.id, "codex");
  assert.equal(isProviderLoginTerminal("antigravity-login"), true);
  assert.equal(isProviderLoginTerminal("codex-login"), true);
  assert.equal(isProviderLoginTerminal("claude-login"), true);
  assert.equal(isProviderLoginTerminal("shell"), false);

  assert.equal(isAgentTerminalTab({ terminalKind: "antigravity" }), true);
  assert.equal(isAgentTerminalTab({ terminalKind: "codex" }), true);
  assert.equal(isAgentTerminalTab({ terminalKind: "shell" }), false);
});

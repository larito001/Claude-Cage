import assert from "node:assert/strict";
import test from "node:test";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { BrowserSpaces, SpaceCookies } from "./browser-spaces.mjs";

test("claude, antigravity, and codex browser spaces isolate contexts, cookies, and routes", async t => {
  const root = await mkdtemp(join(tmpdir(), "browser-spaces-"));
  t.after(() => rm(root, { recursive: true, force: true }));

  function launch() {
    const contexts = new Map();
    const proxies = [];
    let seq = 0;
    const cdp = async (method, params) => {
      if (method === "Target.createBrowserContext") {
        const id = "ctx-" + (++seq);
        contexts.set(id, { cookies: [], proxyServer: params.proxyServer || "" });
        return { browserContextId: id };
      }
      if (method === "Browser.setDownloadBehavior") return {};
      if (method === "Target.createTarget") {
        return { targetId: "tab-" + (++seq) };
      }
      if (method === "Page.navigate") return {};
      if (method === "Storage.getCookies") {
        return { cookies: contexts.get(params.browserContextId)?.cookies || [] };
      }
      if (method === "Storage.setCookies") {
        const ctx = contexts.get(params.browserContextId);
        if (ctx) ctx.cookies = params.cookies.map(c => ({ ...c, domain: c.domain || new URL(c.url).hostname }));
        return {};
      }
      if (method === "Target.disposeBrowserContext") {
        contexts.delete(params.browserContextId);
        return {};
      }
      if (method === "Target.closeTarget") return {};
      throw new Error("Unexpected CDP method: " + method);
    };
    const proxyFactory = async route => {
      const handle = { route, closed: false, options: { proxyServer: "socks5://127.0.0.1:19999" }, close() { this.closed = true; } };
      proxies.push(handle);
      return handle;
    };
    const spaces = new BrowserSpaces(cdp, new SpaceCookies(root), "/tmp/downloads", proxyFactory);
    return { spaces, contexts, proxies };
  }

  const first = launch();
  const routeA = "a".repeat(24);
  const routeB = "b".repeat(24);

  const claudeTab = await first.spaces.open("claude", "https://claude.ai/", async tab => {
    assert.equal(tab.space, "claude");
    assert.equal(tab.incognito, false);
    first.contexts.get(tab.browserContextId).cookies = [
      { name: "sessionKey", value: "claude-only", domain: "claude.ai", path: "/", secure: true, httpOnly: true },
    ];
    return "sid-claude";
  }, { route: routeA });

  const agyTab = await first.spaces.open("antigravity", "https://antigravity.google/", async tab => {
    assert.equal(tab.space, "antigravity");
    first.contexts.get(tab.browserContextId).cookies = [
      { name: "SID", value: "agy-only", domain: "antigravity.google", path: "/", secure: true, httpOnly: true },
    ];
    return "sid-agy";
  });

  const codexTab = await first.spaces.open("codex", "https://chatgpt.com/", async tab => {
    assert.equal(tab.space, "codex");
    first.contexts.get(tab.browserContextId).cookies = [
      { name: "oai", value: "codex-only", domain: "chatgpt.com", path: "/", secure: true, httpOnly: true },
    ];
    return "sid-codex";
  });

  assert.notEqual(claudeTab.id, agyTab.id);
  assert.notEqual(agyTab.id, codexTab.id);
  assert.equal(first.contexts.size, 3);

  // Popup opened from Claude space inherits Claude space and stays out of others.
  const popup = first.spaces.tab({ targetId: "popup-1", openerId: claudeTab.id, url: "https://claude.ai/login" });
  assert.equal(popup.space, "claude");
  assert.equal(popup.incognito, false);

  // Cannot switch exit route while tabs in that space are still open, and
  // failed route switch via open() must not corrupt subsequent opens.
  await assert.rejects(() => first.spaces.setRoute("claude", routeB), /请先关闭此浏览器空间的标签页/);
  await assert.rejects(() => first.spaces.open("claude", "https://claude.ai/new", async () => "sid-2", { route: routeB }), /请先关闭此浏览器空间的标签页/);
  const claudeTab2 = await first.spaces.open("claude", "https://claude.ai/chats", async () => "sid-claude-2");
  assert.equal(claudeTab2.space, "claude");

  await first.spaces.shutdown();
  assert.equal(first.contexts.size, 0);
  assert.equal(first.proxies[0].closed, true);

  //Encrypted on disk: plaintext cookie values must not appear in file bytes.
  const encryptedClaude = await readFile(join(root, "claude"));
  assert.ok(!encryptedClaude.includes(Buffer.from("claude-only")));

  // Restart restores each space's own cookies without cross-space leakage.
  const second = launch();
  for (const [space, expected] of [["claude", ["claude-only"]], ["antigravity", ["agy-only"]], ["codex", ["codex-only"]]]) {
    await second.spaces.open(space, "https://example.com/", async tab => {
      const loaded = second.contexts.get(tab.browserContextId).cookies.map(c => c.value);
      assert.deepEqual(loaded, expected);
      return "sid-" + space;
    });
  }
  await second.spaces.shutdown();
});

test("slow or failed Page.navigate does not block or destroy newly created space tab", async t => {
  const root = await mkdtemp(join(tmpdir(), "browser-spaces-slow-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  let closed = false;
  let disposed = false;
  let navigated = false;
  const cdp = async (method) => {
    if (method === "Target.createBrowserContext") return { browserContextId: "ctx-1" };
    if (method === "Browser.setDownloadBehavior") return {};
    if (method === "Target.createTarget") return { targetId: "tab-1" };
    if (method === "Page.navigate") {
      navigated = true;
      throw new Error("Browser command timed out: Page.navigate");
    }
    if (method === "Target.closeTarget") { closed = true; return {}; }
    if (method === "Target.disposeBrowserContext") { disposed = true; return {}; }
    if (method === "Storage.getCookies") return { cookies: [] };
    throw new Error("Unexpected CDP method: " + method);
  };
  const spaces = new BrowserSpaces(cdp, new SpaceCookies(root), "/tmp/downloads");
  const opened = await spaces.open("claude", "https://claude.ai/", async () => "sid-1");
  await new Promise(resolve => setTimeout(resolve, 10));
  assert.equal(opened.id, "tab-1");
  assert.equal(opened.space, "claude");
  assert.equal(navigated, true);
  assert.equal(closed, false);
  assert.equal(disposed, false);
  assert.equal(spaces.spaces.has("claude"), true);
  await spaces.shutdown();
});

import { join } from "node:path";
import { link, mkdir, readFile, rename, unlink, writeFile } from "node:fs/promises";
import { randomUUID, randomBytes, createCipheriv, createDecipheriv } from "node:crypto";
import { accountProxy } from "./browser-login.mjs";

const validSpaceId = value => typeof value === "string" && /^[a-z][a-z0-9-]{1,31}$/.test(value);

// Persistent encrypted cookie store per Browser Space (Claude / Antigravity / Codex / ...).
// Each space's cookies are bound with AES-256-GCM AAD `space:<id>` so cookies can never
// be transplanted across spaces or mixed with CLI LoginProfiles.
export class SpaceCookies {
  constructor(directory) {
    this.directory = directory;
  }

  async file(space) {
    if (!validSpaceId(space)) throw new Error("浏览器空间无效");
    await mkdir(this.directory, { recursive: true, mode: 0o700 });
    return join(this.directory, space);
  }

  async key() {
    const filename = join(this.directory, ".key");
    try { return await readFile(filename); }
    catch (error) { if (error.code !== "ENOENT") throw error; }
    const key = randomBytes(32), temporary = filename + "." + randomUUID();
    await writeFile(temporary, key, { flag: "wx", mode: 0o600 });
    try {
      await link(temporary, filename);
      return key;
    } catch (error) {
      if (error.code !== "EEXIST") throw error;
      return readFile(filename);
    } finally { await unlink(temporary).catch(() => {}); }
  }

  async load(space) {
    const file = await this.file(space);
    let envelope;
    try { envelope = await readFile(file); }
    catch (error) { if (error.code === "ENOENT") return []; throw error; }
    const decipher = createDecipheriv("aes-256-gcm", await this.key(), envelope.subarray(0, 12));
    decipher.setAAD(Buffer.from("space:" + space));
    decipher.setAuthTag(envelope.subarray(12, 28));
    return JSON.parse(Buffer.concat([decipher.update(envelope.subarray(28)), decipher.final()]).toString("utf8"));
  }

  async save(space, cookies) {
    const file = await this.file(space);
    const iv = randomBytes(12), cipher = createCipheriv("aes-256-gcm", await this.key(), iv);
    cipher.setAAD(Buffer.from("space:" + space));
    const body = Buffer.concat([cipher.update(JSON.stringify(cookies)), cipher.final()]);
    const temporary = file + "." + randomUUID();
    try {
      await writeFile(temporary, Buffer.concat([iv, cipher.getAuthTag(), body]), { flag: "wx", mode: 0o600 });
      await rename(temporary, file);
    } finally { await unlink(temporary).catch(() => {}); }
  }
}

function serializeCookies(cookies) {
  return cookies.filter(cookie => !cookie.partitionKeyOpaque).map(cookie => {
    const { size, session, partitionKeyOpaque, ...params } = cookie;
    if (session) delete params.expires;
    if (!cookie.domain.startsWith(".")) {
      params.url = (cookie.secure ? "https://" : "http://") + cookie.domain + "/";
      delete params.domain;
    }
    return params;
  });
}

// BrowserSpaces isolates provider browser workspaces (Claude, Antigravity, Codex, ...)
// into separate Chromium BrowserContexts with independent cookies, storage, and optional
// per-space strict proxy routes, completely isolated from CLI LoginBrowser and Desktops.
export class BrowserSpaces {
  constructor(cdp, cookies, downloadPath, proxyFactory = accountProxy) {
    this.cdp = cdp;
    this.cookies = cookies;
    this.downloadPath = downloadPath;
    this.proxyFactory = proxyFactory;
    this.spaces = new Map();
    this.contexts = new Map();
    this.targets = new Map();
    this.routes = new Map();
  }

  tab(raw, previous) {
    const entry = this.targets.get(raw.targetId) || this.targets.get(raw.openerId) || this.contexts.get(raw.browserContextId);
    if (!entry && !previous?.space) return raw;
    if (entry && raw.targetId) {
      this.targets.set(raw.targetId, entry);
      entry.targets.add(raw.targetId);
    }
    const space = entry?.space || previous.space;
    return { ...raw, space, incognito: false };
  }

  async ensureContext(space, requestedRoute) {
    if (!validSpaceId(space)) throw new Error("浏览器空间无效");
    const route = requestedRoute !== undefined ? (requestedRoute || "") : (this.routes.get(space) || "");
    if (route && !/^[a-f0-9]{24}$/.test(route)) throw new Error("浏览器空间出口无效");
    let entry = this.spaces.get(space);
    if (entry) {
      if (entry.route !== route) {
        if (entry.targets.size > 0) throw new Error("请先关闭此浏览器空间的标签页，再切换出口");
        await this.disposeSpace(space);
        entry = undefined;
      }
    }
    this.routes.set(space, route);
    if (!entry) {
      const proxy = route ? await this.proxyFactory(route) : undefined;
      let browserContextId = "";
      try {
        ({ browserContextId } = await this.cdp("Target.createBrowserContext", { disposeOnDetach: true, ...proxy?.options }));
        if (this.downloadPath) {
          await this.cdp("Browser.setDownloadBehavior", {
            browserContextId, behavior: "allowAndName",
            downloadPath: this.downloadPath, eventsEnabled: true,
          });
        }
        if (this.cookies) {
          const saved = await this.cookies.load(space);
          if (saved.length) await this.cdp("Storage.setCookies", { browserContextId, cookies: saved });
        }
      } catch (error) {
        if (browserContextId) await this.cdp("Target.disposeBrowserContext", { browserContextId }).catch(() => {});
        proxy?.close();
        throw error;
      }
      entry = { space, route, browserContextId, proxy, targets: new Set() };
      this.spaces.set(space, entry);
      this.contexts.set(browserContextId, entry);
    }
    return entry;
  }

  async open(space, address, activate, { route } = {}) {
    const entry = await this.ensureContext(space, route);
    const { targetId } = await this.cdp("Target.createTarget", {
      browserContextId: entry.browserContextId,
      url: "about:blank",
      newWindow: true,
    });
    entry.targets.add(targetId);
    this.targets.set(targetId, entry);
    let sessionId;
    try {
      sessionId = await activate({
        targetId,
        browserContextId: entry.browserContextId,
        space: entry.space,
        incognito: false,
        type: "page",
        url: "about:blank",
        title: "",
      });
    } catch (error) {
      await this.targetClosed(targetId).catch(() => {});
      await this.cdp("Target.closeTarget", { targetId }).catch(() => {});
      throw error;
    }
    // Kick off navigation without blocking the global mutation queue or
    // destroying the newly created space tab when a remote site is slow.
    void this.cdp("Page.navigate", { url: address }, sessionId, 15000).catch(() => {});
    return { id: targetId, space: entry.space, incognito: false };
  }

  async setRoute(space, route = "") {
    if (!validSpaceId(space)) throw new Error("浏览器空间无效");
    if (route && !/^[a-f0-9]{24}$/.test(route)) throw new Error("浏览器空间出口无效");
    const entry = this.spaces.get(space);
    if (entry && entry.route !== route) {
      if (entry.targets.size > 0) throw new Error("请先关闭此浏览器空间的标签页，再切换出口");
      await this.disposeSpace(space);
    }
    this.routes.set(space, route);
    return { ok: true, space, route };
  }

  async persist(space) {
    const entry = this.spaces.get(space);
    if (!entry || !this.cookies) return;
    const { cookies } = await this.cdp("Storage.getCookies", { browserContextId: entry.browserContextId });
    await this.cookies.save(space, serializeCookies(cookies));
  }

  async disposeSpace(space) {
    const entry = this.spaces.get(space);
    if (!entry) return;
    this.spaces.delete(space);
    this.contexts.delete(entry.browserContextId);
    for (const id of entry.targets) this.targets.delete(id);
    try {
      if (this.cookies) {
        const { cookies } = await this.cdp("Storage.getCookies", { browserContextId: entry.browserContextId });
        await this.cookies.save(space, serializeCookies(cookies));
      }
    } finally {
      try { await this.cdp("Target.disposeBrowserContext", { browserContextId: entry.browserContextId }); }
      finally { entry.proxy?.close(); }
    }
  }

  async targetClosed(targetId) {
    const entry = this.targets.get(targetId);
    if (!entry) return;
    this.targets.delete(targetId);
    entry.targets.delete(targetId);
    if (entry.targets.size === 0) {
      await this.disposeSpace(entry.space);
    } else {
      await this.persist(entry.space).catch(() => {});
    }
  }

  async shutdown() {
    for (const space of [...this.spaces.keys()]) {
      await this.disposeSpace(space).catch(() => {});
    }
  }
}

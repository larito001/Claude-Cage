import React, { useEffect, useRef, useState } from "react";
import { Download, PanelLeftOpen, Plus, RefreshCw } from "lucide-react";
import { api } from "./api";
import { ProviderIcon } from "./agent-icon";
import { SafetyMessage } from "./safety-message";
import { providerSpec } from "./providers.mjs";
import { useWindowVisibility } from "./window-visibility";
import { useTranslation } from "./interface-language.mjs";

export function ProviderDesktopView({
  provider,
  status,
  visible,
  accountDesktops,
  onDetails,
  onExpand,
  sidebarCollapsed,
  onInstalledChange,
}) {
  const tr = useTranslation();
  const spec = providerSpec(provider);
  const windowVisible = useWindowVisibility();
  const screenVisible = visible && windowVisible;

  const [appState, setAppState] = useState(null);
  const [error, setError] = useState("");
  const [statusError, setStatusError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [jobID, setJobID] = useState("");
  const statusRequest = useRef(null);
  const statusRevision = useRef(0);

  const ownJob =
    status?.job?.kind?.startsWith(`${provider}-desktop-`) ||
    status?.job?.kind === `desktop-app-install-${provider}`;
  const busy =
    submitting ||
    Boolean(status?.job?.running) ||
    Boolean(jobID) ||
    Boolean(accountDesktops?.pending);

  const refresh = async (valid = () => true) => {
    if (!provider) return null;
    const revision = statusRevision.current;
    const request = (statusRequest.current ??= api(`/desktop-apps/${provider}`));
    try {
      const value = await request;
      if (!valid() || revision !== statusRevision.current) return null;
      setAppState(value);
      setStatusError("");
      onInstalledChange?.(provider, Boolean(value?.installed));
      return value;
    } catch (e) {
      if (valid() && revision === statusRevision.current) setStatusError(e.message);
      return null;
    } finally {
      if (statusRequest.current === request) statusRequest.current = null;
    }
  };

  const runInstall = async (action = "install") => {
    statusRevision.current++;
    statusRequest.current = null;
    setSubmitting(true);
    setError("");
    try {
      const job = await api(`/desktop-apps/${provider}/${action}`, {});
      setJobID(job.id);
    } catch (e) {
      setError(e.message);
    } finally {
      setSubmitting(false);
    }
  };

  const openOrCreateInstance = async () => {
    setError("");
    const providerCatalog = accountDesktops?.catalog?.providers?.[provider];
    const existing = providerCatalog?.instances?.[0];
    if (existing) {
      await accountDesktops.act(existing, "start", false, false, null, provider);
      return;
    }
    await accountDesktops.act({ id: "" }, "start", false, true, null, provider);
  };

  useEffect(() => {
    setAppState(null);
    setError("");
    setStatusError("");
    setJobID("");
    statusRevision.current++;
    statusRequest.current = null;
  }, [provider]);

  useEffect(() => {
    if (!screenVisible || !status?.vm_running || status?.job?.running) return;
    let disposed = false;
    let timer;
    const poll = async () => {
      await refresh(() => !disposed);
      if (!disposed) timer = setTimeout(poll, 6000);
    };
    void poll();
    return () => {
      disposed = true;
      clearTimeout(timer);
    };
  }, [provider, screenVisible, status?.vm_running, status?.job?.running]);

  useEffect(() => {
    if (!jobID || status?.job?.id !== jobID || status.job.running) return;
    setJobID("");
    if (status.job.error) {
      setError(status.job.error);
      return;
    }
    void (async () => {
      const latest = await refresh();
      await accountDesktops?.refresh?.(true);
      if (latest?.installed) {
        await openOrCreateInstance();
      }
    })();
  }, [status?.job]);

  if (!spec) return null;

  const providerInstances = accountDesktops?.catalog?.providers?.[provider]?.instances || [];

  return (
    <section className="claude-desktop-view" aria-label={spec.desktopLabel}>
      {sidebarCollapsed && (
        <div className="desktop-sidebar-reveal">
          <button
            className="icon-button"
            aria-label={tr("展开侧栏")}
            title={tr("展开侧栏")}
            onClick={onExpand}
          >
            <PanelLeftOpen size={15} />
          </button>
        </div>
      )}
      {(error || statusError) && (
        <div className="connection-banner" role="alert">
          <SafetyMessage message={error || statusError} />
          <button onClick={onDetails}>{tr("操作详情")}</button>
        </div>
      )}
      {busy && ownJob && (
        <div className="desktop-progress" role="status">
          <RefreshCw size={16} className="spin" />
          {status.job.message}
          <button className="text-button" onClick={onDetails}>
            {tr("查看进度")}
          </button>
        </div>
      )}
      <div className="desktop-screen">
        <div className="desktop-empty">
          <ProviderIcon provider={provider} size={40} />
          <h2>
            {appState?.installed
              ? spec.desktopLabel
              : tr("在 Cage 中安装 {0}", spec.desktopLabel)}
          </h2>
          <p>{tr("{0} 实例拥有独立的桌面配置、密钥环与网络隔离，不与浏览器空间或 {1} 共享登录状态。", spec.desktopLabel, spec.cliName)}</p>
          <div className="desktop-actions">
            {appState && (
              <button
                className="primary"
                disabled={busy || !status?.vm_running}
                onClick={() =>
                  appState.installed ? openOrCreateInstance() : runInstall("install")
                }
              >
                {appState.installed ? (
                  providerInstances.length > 0 ? (
                    <ProviderIcon provider={provider} size={16} monochrome />
                  ) : (
                    <Plus size={16} />
                  )
                ) : (
                  <Download size={16} />
                )}
                {appState.installed
                  ? providerInstances.length > 0
                    ? tr("打开 Desktop")
                    : tr("新建 Desktop")
                  : tr("下载并安装")}
              </button>
            )}
            {appState?.installed && (
              <button
                className="quiet-button"
                disabled={busy || !status?.vm_running}
                onClick={() => runInstall("update")}
              >
                {tr("更新 / 修复安装")}
              </button>
            )}
            {!appState && (
              <button
                className="quiet-button"
                disabled={busy}
                onClick={() => refresh()}
              >
                {tr("重新读取状态")}
              </button>
            )}
          </div>
          {appState?.version && (
            <p className="desktop-note">
              <code>{appState.version}</code>
            </p>
          )}
        </div>
      </div>
    </section>
  );
}

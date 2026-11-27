import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Check,
  Globe2,
  Monitor,
  Pencil,
  Plus,
  RefreshCw,
  SlidersHorizontal,
  TerminalSquare,
  Trash2,
} from "lucide-react";
import { api } from "./api";
import { ProviderIcon } from "./agent-icon";
import { InlineSelect } from "./inline-select";
import { ProfileUsage, QuotaClock, UsageNote, useUsageCheck } from "./claude-accounts";
import { PROVIDERS, providerSpec } from "./providers.mjs";
import { translate, useTranslation } from "./interface-language.mjs";

export function useProviderAccounts(provider) {
  const [registry, setRegistry] = useState(null);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const pendingMutation = useRef(0);
  const polling = useRef(false);

  const load = useCallback(
    async (change, visiblePanel = false) => {
      if (!provider) return [];
      if (!change && (pendingMutation.current || (visiblePanel && polling.current))) return;
      const request = ++generation.current;
      if (change) pendingMutation.current++;
      else if (visiblePanel) polling.current = true;
      try {
        const endpoint = visiblePanel
          ? `/provider-accounts/${provider}?panel=visible`
          : `/provider-accounts/${provider}`;
        const value = await api(endpoint, change);
        if (!Array.isArray(value?.accounts)) {
          throw new Error(translate("无法读取 {0} 账号列表", provider));
        }
        if (generation.current === request) {
          setRegistry(value);
          setError("");
        }
        return value.accounts;
      } catch (e) {
        if (generation.current === request) setError(e.message);
        if (change) throw e;
      } finally {
        if (change) pendingMutation.current--;
        else if (visiblePanel) polling.current = false;
      }
    },
    [provider],
  );

  useEffect(() => {
    void load();
    return () => {
      generation.current++;
    };
  }, [load]);

  return {
    accounts: registry?.accounts ?? null,
    detected: registry?.detected ?? registry?.default_profile ?? null,
    desktopEmails: registry?.desktop_emails ?? [],
    detectionStatus: registry?.detection_status || "",
    detectionError: registry?.detection_error || "",
    usageRunning: Boolean(registry?.usage_panel_running),
    usageUnavailable: registry?.usage_unavailable_reason || "",
    scheduling: registry?.scheduling || { mode: "balanced", switch_policy: "ask" },
    loginNotice: registry?.login_notice || "",
    error,
    load,
  };
}

export function AccountProviderTabs({ activeProvider, onSelect, counts = {} }) {
  const tr = useTranslation();
  return (
    <div className="provider-account-tabs" role="tablist" aria-label={tr("AI 平台账号切换")}>
      {PROVIDERS.map((item) => {
        const selected = activeProvider === item.id;
        const count = counts[item.id];
        return (
          <button
            key={item.id}
            type="button"
            role="tab"
            aria-selected={selected}
            className={`provider-account-tab provider-${item.id} ${selected ? "selected" : ""}`}
            onClick={() => onSelect(item.id)}
          >
            <ProviderIcon provider={item.id} size={15} />
            <span>{item.name}</span>
            {typeof count === "number" && <small>{count}</small>}
          </button>
        );
      })}
    </div>
  );
}

const statusLabels = {
  pending: "等待登录",
  ready: "已登录",
  signed_out: "需要登录",
  duplicate: "账号已在列表中",
  changed: "登录身份已改变",
};

function accountCaption(profile) {
  const label = profile.label || profile.email || translate("未命名账号");
  return profile.number ? `#${String(profile.number).padStart(2, "0")} ${label}` : label;
}

function ProviderBadgeAvatar({ spec }) {
  return (
    <span
      className={`claude-profile-avatar provider-avatar provider-avatar-${spec.id}`}
      aria-hidden="true"
    >
      <ProviderIcon provider={spec.id} size={22} />
    </span>
  );
}

function DetectedProviderProfile({
  spec,
  profile,
  usageEnvironment,
  busy,
  perform,
  change,
  onJoin,
  onDismiss,
}) {
  const tr = useTranslation();
  const usage = useUsageCheck(profile, usageEnvironment, perform, change);
  return (
    <article className="claude-profile claude-detected" aria-label={profile.email || profile.label}>
      <div className="claude-profile-heading">
        <ProviderBadgeAvatar spec={spec} />
        <div className="claude-profile-identity">
          <div className="claude-profile-name">
            <h4 title={profile.email || profile.label}>{profile.email || profile.label}</h4>
            {profile.plan && <span className="claude-profile-plan">{profile.plan}</span>}
          </div>
          <div className="claude-profile-meta">
            <span className="claude-profile-status">
              {tr("普通终端 · 已发现，未加入")}
            </span>
          </div>
        </div>
      </div>
      <ProfileUsage quota={profile.quota} state={usage.state} />
      <div className="claude-profile-actions">
        <button className="quiet-button" disabled={busy} onClick={onJoin}>
          <Plus size={14} />
          {tr("加入调度")}
        </button>
        <button
          className="quiet-button"
          disabled={busy || Boolean(usageEnvironment) || usage.state === "checking"}
          onClick={usage.check}
        >
          <RefreshCw size={14} className={usage.state === "checking" ? "spin" : undefined} />
          {tr("检查状态")}
        </button>
        <UsageNote profile={profile} state={usage.state} />
        <button className="text-button" disabled={busy} onClick={onDismiss}>
          {tr("取消")}
        </button>
      </div>
    </article>
  );
}

function ProviderProfileCard({
  spec,
  proxySources,
  desktopNetwork,
  profile,
  environment,
  usageEnvironment,
  busy,
  active,
  perform,
  change,
  login,
  cancelDraft,
  removeAccount,
  onOpenDesktop,
}) {
  const tr = useTranslation();
  const [renaming, setRenaming] = useState(false);
  const [configOpen, setConfigOpen] = useState(false);
  const [label, setLabel] = useState("");
  const displayName = profile.label || profile.email || spec.name;
  const isDraft =
    !profile.email &&
    !profile.subject &&
    ["pending", "signed_out", "duplicate"].includes(profile.status);
  const savedLabel = label.trim() || profile.email || profile.label;
  const usage = useUsageCheck(profile, usageEnvironment, perform, change);

  const boundNodeName = useMemo(() => {
    if (!profile.proxy_node_id) return "";
    for (const source of proxySources) {
      const found = source.nodes?.find((node) => node.id === profile.proxy_node_id);
      if (found) return found.name;
    }
    return tr("已绑定节点不可用");
  }, [profile.proxy_node_id, proxySources, tr]);

  const cliStatusTitle =
    `${spec.cliCommand} · ` +
    (statusLabels[profile.status] ? tr(statusLabels[profile.status]) : tr("状态未知"));

  return (
    <article
      className="claude-profile"
      aria-label={accountCaption(profile)}
      data-enabled={profile.enabled}
    >
      <div className="claude-profile-heading">
        <ProviderBadgeAvatar spec={spec} />
        <div className="claude-profile-identity">
          {renaming ? (
            <form
              className="claude-profile-rename"
              onSubmit={(e) => {
                e.preventDefault();
                perform(async () => {
                  await change({ action: "update", id: profile.id, label: savedLabel });
                  setRenaming(false);
                });
              }}
            >
              <input
                autoFocus
                aria-label={tr("账号备注")}
                maxLength={64}
                placeholder={profile.email || tr("账号名称")}
                value={label}
                onChange={(e) => setLabel(e.target.value)}
              />
              <button disabled={busy || !savedLabel} aria-label={tr("保存备注")}>
                <Check size={15} />
              </button>
              <button type="button" disabled={busy} onClick={() => setRenaming(false)}>
                {tr("取消")}
              </button>
            </form>
          ) : (
            <div className="claude-profile-name">
              <h4 title={accountCaption(profile)}>{accountCaption(profile)}</h4>
              {profile.plan && <span className="claude-profile-plan">{profile.plan}</span>}
              {active > 0 && (
                <span
                  className="claude-profile-active-chip"
                  title={`${active} ` + tr("个终端仍在运行")}
                >
                  {active} {tr("运行中")}
                </span>
              )}
              <button
                type="button"
                className="icon-button claude-profile-edit"
                disabled={busy}
                aria-label={tr("修改 {0} 的备注", displayName)}
                title={tr("账号备注")}
                onClick={() => {
                  setLabel(profile.label === profile.email ? "" : profile.label || "");
                  setRenaming(true);
                }}
              >
                <Pencil size={13} />
              </button>
            </div>
          )}
          <div className="claude-profile-meta">
            <span
              className="claude-status-pill"
              data-ready={profile.status === "ready"}
              title={cliStatusTitle}
            >
              {spec.cliCommand}
            </span>
            {profile.auth_mode && (
              <span className="claude-status-pill" data-ready="true">
                {profile.auth_mode}
              </span>
            )}
            {profile.email &&
              (profile.email !== displayName || profile.organization) && (
                <span
                  className="claude-profile-email"
                  title={[profile.email, profile.organization].filter(Boolean).join(" · ")}
                >
                  {[profile.email !== displayName && profile.email, profile.organization]
                    .filter(Boolean)
                    .join(" · ")}
                </span>
              )}
          </div>
        </div>
        <label
          className="check-line claude-participation"
          title={profile.enabled ? tr("参与分配") : tr("仅手动选择")}
        >
          <input
            type="checkbox"
            aria-label={tr("参与自动分配")}
            checked={Boolean(profile.enabled)}
            disabled={busy}
            onChange={(e) => {
              const enabled = e.target.checked;
              perform(() => change({ action: "update", id: profile.id, enabled }));
            }}
          />
          <span className="claude-switch-track" aria-hidden="true" />
        </label>
      </div>

      <ProfileUsage quota={profile.quota} state={usage.state} />

      {desktopNetwork && boundNodeName && !configOpen && (
        <div className="claude-config-chips">
          <button
            type="button"
            className="claude-config-chip"
            onClick={() => setConfigOpen(true)}
            title={tr("账号出口")}
          >
            <Globe2 size={12} />
            <span>{boundNodeName}</span>
          </button>
        </div>
      )}

      {configOpen && desktopNetwork && (
        <div className="claude-profile-config">
          <label className="claude-config-field">
            <span>{tr("账号出口")}</span>
            <InlineSelect
              inline
              aria-label={displayName + " " + tr("的代理节点")}
              value={profile.proxy_node_id || ""}
              disabled={busy}
              onValueChange={(node) =>
                perform(() =>
                  change({ action: "update", id: profile.id, proxy_node_id: node }),
                )
              }
            >
              <option value="">{tr("跟随环境出口")}</option>
              {profile.proxy_node_id &&
                !proxySources.some((source) =>
                  source.nodes?.some((node) => node.id === profile.proxy_node_id),
                ) && (
                  <option value={profile.proxy_node_id} disabled>
                    {tr("已绑定节点不可用")}
                  </option>
                )}
              {proxySources.map((source) => (
                <optgroup label={source.name} key={source.id}>
                  {(source.nodes || []).map((node) => (
                    <option
                      value={node.id}
                      key={node.id}
                      disabled={Boolean(node.blocked_reason)}
                    >
                      {node.blocked_reason ? tr("{0}（需迁移）", node.name) : node.name}
                    </option>
                  ))}
                </optgroup>
              ))}
            </InlineSelect>
          </label>
        </div>
      )}

      {profile.status === "changed" && (
        <p className="field-help">{tr("请用原账号重新登录。")}</p>
      )}
      {profile.status === "duplicate" && (
        <p className="field-help">{tr("同一订阅只参与一次分配。请在此位置登录另一个账号。")}</p>
      )}
      {profile.note && <p className="field-help">{profile.note}</p>}

      <div className="claude-profile-actions">
        <button
          className="quiet-button"
          disabled={busy || Boolean(environment) || active > 0}
          onClick={() => perform(() => login(spec.id, profile.id))}
        >
          <TerminalSquare size={14} />
          {profile.status === "ready" ? tr("重新授权 CLI") : tr("授权 CLI")}
        </button>
        {onOpenDesktop && !isDraft && (
          <button
            type="button"
            className="quiet-button"
            disabled={busy || Boolean(environment)}
            onClick={() => perform(() => onOpenDesktop(spec.id, profile))}
            title={tr("打开 Desktop")}
          >
            <Monitor size={14} />
            {tr("打开 Desktop")}
          </button>
        )}
        <button
          className="quiet-button"
          disabled={busy || Boolean(usageEnvironment) || usage.state === "checking"}
          onClick={usage.check}
        >
          <RefreshCw size={14} className={usage.state === "checking" ? "spin" : undefined} />
          {tr("检查状态")}
        </button>
        {desktopNetwork && (
          <button
            type="button"
            className={`quiet-button claude-config-toggle ${configOpen ? "open" : ""}`}
            aria-expanded={configOpen}
            onClick={() => setConfigOpen(!configOpen)}
          >
            <SlidersHorizontal size={13} />
            {tr("设置")}
            {Boolean(boundNodeName) && <i className="claude-config-dot" aria-hidden="true" />}
          </button>
        )}
        <UsageNote profile={profile} state={usage.state} />
        {isDraft ? (
          !active && (
            <button
              className="text-button"
              disabled={busy}
              onClick={() => perform(() => cancelDraft(profile))}
            >
              {tr("撤销添加")}
            </button>
          )
        ) : (
          <button
            type="button"
            className="icon-button claude-profile-delete"
            disabled={busy || active > 0}
            aria-label={tr("删除账号")}
            title={active > 0 ? tr("请先关闭该账号的终端，再删除账号") : tr("删除账号")}
            onClick={(event) => perform(() => removeAccount(profile, event.currentTarget))}
          >
            <Trash2 size={14} />
          </button>
        )}
      </div>
    </article>
  );
}

export function ProviderAccountsPanel({
  provider,
  proxySources = [],
  desktopNetwork,
  store,
  running,
  environmentBusy,
  tabs = [],
  login,
  enrollmentID,
  cancelLogin,
  onConfirm,
  onOpenDesktop,
}) {
  const tr = useTranslation();
  const spec = providerSpec(provider);
  const {
    accounts,
    detected,
    desktopEmails,
    detectionStatus,
    detectionError,
    usageRunning,
    usageUnavailable,
    loginNotice,
    load,
    error,
  } = store;

  const [busy, setBusy] = useState(false);
  const [adding, setAdding] = useState(false);
  const [schedulingOpen, setSchedulingOpen] = useState(false);
  const [node, setNode] = useState("");
  const [deviceAuth, setDeviceAuth] = useState(false);
  const [requestingUsage, setRequestingUsage] = useState(false);
  const [actionError, setActionError] = useState("");

  const visibleProfiles = useMemo(
    () => (detected ? [detected, ...(accounts || [])] : accounts),
    [accounts, detected],
  );

  const nodeAvailable =
    !node ||
    proxySources.some((source) =>
      source.nodes?.some((n) => n.id === node && !n.blocked_reason),
    );
  const environment =
    !running || detectionStatus === "offline"
      ? "offline"
      : detectionStatus === "update_required"
        ? "update_required"
        : "";
  const usageEnvironment = environment || (usageUnavailable ? "blocked" : "");
  const checking =
    !usageEnvironment &&
    (usageRunning || visibleProfiles?.some((profile) => profile.usage_checking));

  const activeCounts = useMemo(() => {
    const counts = new Map();
    for (const tab of tabs) {
      if (tab.closed) continue;
      if (tab.provider !== provider && tab.terminalKind !== spec?.terminalKind) continue;
      const accountID = tab.provider_account_id || tab.claude_account_id;
      if (!accountID) continue;
      counts.set(accountID, (counts.get(accountID) || 0) + 1);
    }
    return counts;
  }, [tabs, provider, spec]);

  useEffect(() => {
    let requested = false;
    let mounted = true;
    const openPanel = () => {
      if (requested || document.hidden) return;
      requested = true;
      setRequestingUsage(Boolean(running));
      if (running) {
        load({ action: "usage_panel" })
          .catch(() => {})
          .finally(() => {
            if (mounted) setRequestingUsage(false);
          });
      } else {
        void load();
      }
    };
    openPanel();
    document.addEventListener("visibilitychange", openPanel);
    return () => {
      mounted = false;
      document.removeEventListener("visibilitychange", openPanel);
    };
  }, [load, running, provider]);

  useEffect(() => {
    const timer = setInterval(
      () => {
        if (!document.hidden) void load(undefined, true);
      },
      checking ? 2000 : 12000,
    );
    return () => clearInterval(timer);
  }, [load, checking]);

  const perform = async (operation) => {
    if (busy) return;
    setBusy(true);
    setActionError("");
    try {
      await operation();
    } catch (e) {
      setActionError(e.message);
    } finally {
      setBusy(false);
    }
  };

  const cancelDraft = async (profile) => {
    if (
      await onConfirm({
        title: translate("撤销添加「{0}」？", profile.label || spec.name),
        body: "此账号会从列表中移除，虚拟机中的资料会保留。",
        confirm: "撤销添加",
      })
    ) {
      await load({ action: "cancel", id: profile.id });
    }
  };

  const removeAccount = async (profile, anchor) => {
    const displayName = profile.label || profile.email || spec.name;
    await onConfirm({
      title: translate("删除账号「{0}」？", displayName),
      body: translate("删除后会从账号管理与调度中移除此账号。虚拟机内的会话历史和资料会保留。"),
      confirm: translate("删除账号"),
      pendingLabel: translate("正在删除…"),
      danger: true,
      anchor,
      submit: async () => {
        await load({ action: "delete", id: profile.id });
      },
    });
  };

  const unlinkedDesktopEmails = useMemo(() => {
    const known = new Set(
      (accounts || [])
        .map((item) => (item.email || "").toLowerCase())
        .filter(Boolean),
    );
    return (desktopEmails || []).filter((email) => !known.has(email.toLowerCase()));
  }, [accounts, desktopEmails]);

  if (!spec) return null;

  return (
    <section className="claude-profiles" aria-label={tr("{0} 账号管理", spec.name)}>
      <div className="claude-toolbar">
        <div className="claude-toolbar-title">
          <h4>
            {tr("账号列表")}
            <span>{(detected ? 1 : 0) + (accounts?.length ?? 0)}</span>
          </h4>
          {requestingUsage || checking ? (
            <small className="claude-toolbar-status" role="status">
              <RefreshCw size={12} className="spin" />
              {tr("正在查询账号用量…")}
            </small>
          ) : (
            accounts?.length > 0 && (
              <small className="claude-toolbar-status">
                {accounts.filter((a) => a.enabled).length} {tr("个启用调度")}
              </small>
            )
          )}
        </div>
        <div className="claude-toolbar-actions">
          <button
            type="button"
            className={`claude-scheduling-trigger ${schedulingOpen ? "open" : ""}`}
            aria-expanded={schedulingOpen}
            onClick={() => setSchedulingOpen(!schedulingOpen)}
          >
            <SlidersHorizontal size={14} />
            <span>
              {store.scheduling.mode === "sequential" ? tr("顺序优先") : tr("均衡分配")}
            </span>
          </button>
          <button
            className="primary"
            disabled={busy || Boolean(enrollmentID) || environmentBusy || Boolean(environment)}
            aria-expanded={adding}
            onClick={() => setAdding(!adding)}
          >
            <Plus size={15} />
            {tr("添加账号")}
          </button>
        </div>
      </div>

      {schedulingOpen && (
        <div className="claude-scheduling-card">
          <div className="claude-scheduling-grid">
            <label>
              {tr("新会话分配")}
              <InlineSelect
                inline
                aria-label={tr("新会话分配")}
                value={store.scheduling.mode}
                disabled={busy}
                onValueChange={(mode) =>
                  perform(() =>
                    load({ action: "scheduling", scheduling: { ...store.scheduling, mode } }),
                  )
                }
              >
                <option value="balanced">{tr("均衡分配")}</option>
                <option value="sequential">{tr("按列表顺序优先")}</option>
              </InlineSelect>
            </label>
          </div>
          {store.scheduling.mode === "sequential" && accounts?.length > 1 && (
            <div className="claude-account-order" aria-label={tr("账号优先顺序")}>
              {accounts.map((profile, index) => (
                <div key={profile.id}>
                  <span>
                    {index + 1}. {accountCaption(profile)}
                  </span>
                  <button
                    disabled={busy || index === 0}
                    aria-label={tr("提高 {0} 的优先级", profile.label || profile.email)}
                    onClick={() => {
                      const order = accounts.map((a) => a.id);
                      [order[index - 1], order[index]] = [order[index], order[index - 1]];
                      perform(() =>
                        load({ action: "scheduling", scheduling: store.scheduling, order }),
                      );
                    }}
                  >
                    {tr("上移")}
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {adding && (
        <form
          className="claude-add-form"
          onSubmit={(e) => {
            e.preventDefault();
            if (!nodeAvailable) return;
            perform(async () => {
              await login(spec.id, undefined, node, deviceAuth);
              setAdding(false);
            });
          }}
        >
          {desktopNetwork && proxySources.length > 0 && (
            <label>
              {tr("账号出口")}
              <InlineSelect
                inline
                aria-label={tr("新账号出口")}
                value={node}
                onValueChange={setNode}
              >
                <option value="">{tr("跟随环境出口")}</option>
                {proxySources.map((source) => (
                  <optgroup key={source.id} label={source.name}>
                    {(source.nodes || []).map((n) => (
                      <option value={n.id} key={n.id} disabled={Boolean(n.blocked_reason)}>
                        {n.blocked_reason ? tr("{0}（需迁移）", n.name) : n.name}
                      </option>
                    ))}
                  </optgroup>
                ))}
              </InlineSelect>
            </label>
          )}
          {spec.supportsDeviceAuth && (
            <label className="check-line">
              <input
                type="checkbox"
                checked={deviceAuth}
                onChange={(e) => setDeviceAuth(e.target.checked)}
              />
              {tr("设备码登录")}
            </label>
          )}
          <div>
            <button className="primary" disabled={busy || Boolean(environment) || !nodeAvailable}>
              {tr("开始授权")}
            </button>
            <button type="button" onClick={() => setAdding(false)}>
              {tr("取消")}
            </button>
          </div>
        </form>
      )}

      {enrollmentID && (
        <div className="claude-add-profile">
          <span role="status">{tr("正在等待 {0} 授权…", spec.name)}</span>
          <button onClick={() => perform(cancelLogin)}>{tr("取消授权")}</button>
        </div>
      )}

      {(actionError || error) && (
        <p className="account-error" role="alert">
          {actionError || error}
        </p>
      )}
      {loginNotice && (
        <p className="claude-profile-note" role="status">
          {loginNotice}
        </p>
      )}
      {detectionError && (
        <p className="account-error" role="alert">
          {detectionError}
        </p>
      )}
      {usageUnavailable && !environment && (
        <p className="claude-profile-note" role="status">
          {usageUnavailable}
        </p>
      )}

      {unlinkedDesktopEmails.length > 0 && (
        <p className="claude-profile-note" role="status">
          {tr("已在独立 Desktop 发现邮箱：{0}", unlinkedDesktopEmails.join(", "))}
        </p>
      )}

      {!running ? (
        <p className="claude-profile-note">
          {tr("启动 SuperCage 环境后可检测普通终端的登录，也可登录新账号。")}
        </p>
      ) : (
        detectionStatus === "offline" && (
          <p className="claude-profile-note">
            {tr("工作环境就绪后可检测和登录账号。")}
          </p>
        )
      )}

      <div className="claude-profile-list" role="region" aria-label={tr("账号列表")} tabIndex={0}>
        {accounts === null && !error && <p role="status">{tr("正在读取账号…")}</p>}
        {accounts?.length === 0 && !detected && (
          <div className="claude-profile-empty">
            <ProviderBadgeAvatar spec={spec} />
            <p>{tr("尚未添加账号")}</p>
            <small>{tr("登录成功后，账号会出现在这里。")}</small>
          </div>
        )}
        <QuotaClock accounts={visibleProfiles}>
          {detected && (
            <DetectedProviderProfile
              spec={spec}
              profile={detected}
              usageEnvironment={usageEnvironment}
              busy={busy || environmentBusy || Boolean(environment)}
              perform={perform}
              change={load}
              onJoin={() =>
                perform(() =>
                  load({
                    action: "join",
                    identity: detected.identity || detected.subject || detected.email,
                  }),
                )
              }
              onDismiss={() =>
                perform(() =>
                  load({
                    action: "dismiss_default",
                    identity: detected.identity || detected.subject || detected.email,
                  }),
                )
              }
            />
          )}
          {accounts?.map((profile) => (
            <ProviderProfileCard
              key={profile.id}
              spec={spec}
              proxySources={proxySources}
              desktopNetwork={desktopNetwork}
              profile={profile}
              environment={environment}
              usageEnvironment={usageEnvironment}
              busy={busy || environmentBusy}
              active={activeCounts.get(profile.id) || 0}
              perform={perform}
              change={load}
              login={login}
              cancelDraft={cancelDraft}
              removeAccount={removeAccount}
              onOpenDesktop={onOpenDesktop}
            />
          ))}
        </QuotaClock>
      </div>
    </section>
  );
}

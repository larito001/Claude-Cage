import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Globe2, Layers, Shield } from "lucide-react";
import { api } from "./api";
import { InlineSelect } from "./inline-select";
import { ProviderIcon } from "./agent-icon";
import { PROVIDERS, browserSpaceSpec } from "./providers.mjs";
import { useTranslation } from "./interface-language.mjs";

export function useBrowserSpaces(ready) {
  const [spaces, setSpaces] = useState([]);
  const [loading, setLoading] = useState(false);
  const seq = useRef(0);

  const refresh = useCallback(async () => {
    const current = ++seq.current;
    try {
      const data = await api("/browser/spaces");
      if (current === seq.current && Array.isArray(data?.spaces)) {
        setSpaces(data.spaces);
      }
      return data?.spaces || [];
    } catch {
      return [];
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [ready, refresh]);

  const setRoute = useCallback(async (space, proxyNodeID) => {
    setLoading(true);
    try {
      const data = await api("/browser/spaces", {
        action: "route",
        space,
        proxy_node_id: proxyNodeID || "",
      });
      if (Array.isArray(data?.spaces)) setSpaces(data.spaces);
      return data?.spaces || [];
    } finally {
      setLoading(false);
    }
  }, []);

  const byID = useMemo(
    () => new Map(spaces.map((item) => [item.id, item])),
    [spaces],
  );

  return { spaces, byID, loading, refresh, setRoute };
}

export function BrowserSpaceBadge({ space }) {
  const tr = useTranslation();
  const spec = browserSpaceSpec(space);
  if (!spec) return null;
  return (
    <span
      className={`browser-space-badge browser-space-${spec.id}`}
      title={tr("{0} 浏览器空间", spec.name)}
    >
      <ProviderIcon provider={spec.id} size={13} />
      {tr("{0} 浏览器空间", spec.name)}
    </span>
  );
}

export function BrowserSpaceMenuSection({
  tab,
  browserSpaces,
  proxySources = [],
  ready,
  busy,
  onOpenSpace,
  onStatus,
}) {
  const tr = useTranslation();
  const currentSpace = tab?.incognito ? "" : tab?.space || "";
  const spec = browserSpaceSpec(currentSpace);
  const spaceInfo = currentSpace ? browserSpaces?.byID?.get(currentSpace) : null;
  const activeRoute = spaceInfo?.proxy_node_id || "";
  const hasNodes = proxySources.some((source) => source.nodes?.length > 0);

  const handleRouteChange = async (nextNodeID) => {
    if (!currentSpace || !browserSpaces) return;
    try {
      await browserSpaces.setRoute(currentSpace, nextNodeID);
      onStatus?.(
        nextNodeID
          ? tr("{0} 空间已切换出口节点", spec?.name || currentSpace)
          : tr("{0} 空间已恢复跟随环境出口", spec?.name || currentSpace),
      );
    } catch (error) {
      onStatus?.(error.message, "error");
    }
  };

  return (
    <div className="browser-space-menu-section" role="group" aria-label={tr("隔离浏览器空间")}>
      <div className="browser-space-menu-header">
        <Layers size={14} aria-hidden="true" />
        <span>{tr("隔离浏览器空间")}</span>
      </div>
      <div className="browser-space-grid">
        <button
          type="button"
          className={`browser-space-chip ${!tab?.incognito && !currentSpace ? "active" : ""}`}
          role="menuitem"
          disabled={!ready || busy}
          onClick={() => onOpenSpace?.("", undefined)}
        >
          <Globe2 size={13} aria-hidden="true" />
          <span>{tr("通用空间")}</span>
        </button>
        {PROVIDERS.map((provider) => {
          const info = browserSpaces?.byID?.get(provider.id);
          const count = info?.active_tabs || 0;
          const isCurrent = !tab?.incognito && currentSpace === provider.id;
          return (
            <button
              key={provider.id}
              type="button"
              className={`browser-space-chip browser-space-${provider.id} ${isCurrent ? "active" : ""}`}
              role="menuitem"
              disabled={!ready || busy}
              onClick={() => onOpenSpace?.(provider.id, provider.homeURL)}
              title={tr("{0} 浏览器空间", provider.name)}
            >
              <ProviderIcon provider={provider.id} size={13} />
              <span>{provider.shortBadge}</span>
              {count > 0 && <small>{count}</small>}
            </button>
          );
        })}
      </div>
      {spec && hasNodes && (
        <div
          className="browser-space-route"
          onClick={(event) => event.stopPropagation()}
          onPointerDown={(event) => event.stopPropagation()}
        >
          <label>
            <span className="browser-space-route-label">
              <Shield size={12} aria-hidden="true" />
              {tr("账号出口")}
            </span>
            <InlineSelect
              inline
              aria-label={tr("账号出口")}
              value={activeRoute}
              disabled={!ready || busy || browserSpaces?.loading}
              onValueChange={handleRouteChange}
            >
              <option value="">{tr("跟随环境出口")}</option>
              {proxySources.map((source) => (
                <optgroup key={source.id} label={source.name}>
                  {(source.nodes || []).map((node) => (
                    <option key={node.id} value={node.id}>
                      {node.name}
                    </option>
                  ))}
                </optgroup>
              ))}
            </InlineSelect>
          </label>
          {spaceInfo?.active_tabs > 0 && (
            <small className="field-help">
              {tr("切换此空间的出口节点前，请先关闭该空间已打开的标签页。")}
            </small>
          )}
        </div>
      )}
    </div>
  );
}

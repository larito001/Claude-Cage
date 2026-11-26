import React, { useState } from "react";
import { api, nativeAvailable } from "./api";
import { isRemoteClient } from "./client-context.mjs";
import { InlineSelect } from "./inline-select";
import { useTranslation } from "./interface-language.mjs";
import { isClientProjectPath, projectGroups, projectName } from "./workspace-state.mjs";

export function DesktopProjectFolder({ status, desktop, onStatus }) {
  const tr = useTranslation();
  const [pending, setPending] = useState(false);
  if (!status?.ready || !nativeAvailable() || isRemoteClient()) return null;
  const folders = [...new Set(projectGroups(status).flatMap(group => group.folders))]
    .filter(path => path && path !== status.environment_directory && !isClientProjectPath(path));
  if (!folders.length) return null;
  async function open(workspace) {
    setPending(true);
    try {
      await api("/input/files/desktop/project-folder", { desktop, workspace });
    } catch (error) {
      onStatus?.(error.message);
    } finally {
      setPending(false);
    }
  }
  const label = tr("在文件夹中显示");
  if (folders.length === 1) {
    return <button disabled={pending} title={folders[0]} onClick={() => open(folders[0])}>{label} · {projectName(folders[0])}</button>;
  }
  return <InlineSelect value="" displayLabel={label} aria-label={label} disabled={pending} onValueChange={open}>
    {folders.map(path => <option key={path} value={path}>{path}</option>)}
  </InlineSelect>;
}

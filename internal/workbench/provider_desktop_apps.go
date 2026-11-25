package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/usererror"
)

type desktopAppStatus struct {
	Provider  string `json:"provider"`
	Name      string `json:"name"`
	Package   string `json:"package"`
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
}

func desktopAppsSnapshotScript() string {
	providers := cage.Providers()
	pairs := make([]string, 0, len(providers))
	for _, p := range providers {
		pairs = append(pairs, fmt.Sprintf("(%q, %q, %q)", p.ID, p.Desktop.Name, p.Desktop.Package))
	}
	py := `import json, subprocess
def pkg_info(name):
    out = subprocess.run(['dpkg-query', '-W', '-f=${Status}\n${Version}', name], capture_output=True, text=True).stdout.strip().splitlines()
    installed = bool(out and out[0] == 'install ok installed')
    return installed, (out[1] if installed and len(out) > 1 else '')
apps = {}
for pid, name, pkg in [` + joinStrings(pairs, ", ") + `]:
    installed, version = pkg_info(pkg)
    apps[pid] = {'provider': pid, 'name': name, 'package': pkg, 'installed': installed, 'version': version}
print(json.dumps({'apps': apps}))
`
	return "python3 -I -c " + shellescape.Quote(py)
}

func joinStrings(items []string, sep string) string {
	if len(items) == 0 {
		return ""
	}
	out := items[0]
	for _, item := range items[1:] {
		out += sep + item
	}
	return out
}

func (s *Server) listDesktopApps(w http.ResponseWriter, r *http.Request) {
	if !s.vmRunning() {
		http.Error(w, "请先启动 Cage 环境", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	b, err := s.guestQuick(ctx, desktopAppsSnapshotScript())
	if err != nil {
		_ = diagnostics.New(s.profile).Record("warn", "desktop-apps", fmt.Sprintf("read state: %v", err))
		http.Error(w, usererror.Wrap("读取 Desktop 应用状态", err, "请确认环境仍在运行并稍后重试。").Error(), http.StatusBadGateway)
		return
	}
	var result struct {
		Apps map[string]desktopAppStatus `json:"apps"`
	}
	if json.Unmarshal(b, &result) != nil || result.Apps == nil {
		http.Error(w, "读取 Desktop 应用状态失败：返回格式无效", http.StatusBadGateway)
		return
	}
	if provider := r.PathValue("provider"); provider != "" {
		app, ok := result.Apps[provider]
		if !ok {
			http.NotFound(w, r)
			return
		}
		jsonReply(w, app)
		return
	}
	jsonReply(w, result)
}

func (s *Server) desktopAppAction(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	action := r.PathValue("action")
	if action != "install" && action != "update" {
		http.NotFound(w, r)
		return
	}
	if provider == "claude" {
		s.claudeDesktopAction(w, r)
		return
	}
	spec, ok := cage.LookupProvider(provider)
	installScript, hasScript := cage.DesktopInstallScriptFor(provider)
	if !ok || !hasScript {
		http.NotFound(w, r)
		return
	}
	var input struct{}
	if !readJSON(w, r, &input) {
		return
	}
	if !s.vmRunning() {
		http.Error(w, "请先启动 Cage 环境", http.StatusConflict)
		return
	}
	s.launchRequest(w, r, "desktop-app-install-"+provider, func() error {
		s.mu.Lock()
		allowed := s.toolsNetworkAllowedLocked()
		tls12Compat := s.preferences.InstallTLS12Compat
		s.mu.Unlock()
		if !allowed {
			return errors.New("工作台正在退出，请稍后重试安装")
		}
		monitorContext, stopMonitor := context.WithCancel(s.operationContext())
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-monitorContext.Done():
					return
				case <-ticker.C:
					s.monitorSafety()
				}
			}
		}()
		defer func() {
			stopMonitor()
			s.mu.Lock()
			if s.monitorCancel != nil {
				s.monitorCancel()
			}
			s.mu.Unlock()
			<-done
		}()
		s.setMessage(fmt.Sprintf("正在下载并安装 %s，可在操作详情查看进度…", spec.Desktop.Name))
		stopScript := ""
		if action == "update" {
			stopScript = fmt.Sprintf(`python3 -I - <<'STOP_PROVIDER_DESKTOPS'
from pathlib import Path
import subprocess
base = Path.home() / ".local/share/desktop-profiles"
if base.is_dir():
    for d in base.iterdir():
        try:
            prov = (d / "provider").read_text(encoding="utf-8").strip()
        except OSError:
            prov = ""
        if prov == %q:
            subprocess.run(["sudo", "systemctl", "stop", f"desktop-{d.name}.service"], check=False)
STOP_PROVIDER_DESKTOPS
`, provider)
		}
		script := cage.InstallTLS12Prelude(tls12Compat) + stopScript + installScript
		return RunMaintenanceScript(s.operationContext(), s.configPath, script, s.jobs.execution(), s.jobs.execution())
	})
}

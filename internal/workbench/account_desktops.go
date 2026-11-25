package workbench

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/containerenv"
)

//go:embed linux-account-desktop.sh
var linuxAccountDesktop string

type desktopEntry struct {
	Provider   string `json:"provider,omitempty"`
	Generation int    `json:"generation,omitempty"`
	AccountID  string `json:"account_id,omitempty"`
	ID         string `json:"id"`
	Number     uint32 `json:"number"`
	Label      string `json:"label"`
	Running    bool   `json:"running"`
	Dedicated  bool   `json:"dedicated"`
}

func (s *Server) accountDesktopsSupported() bool {
	return !containerenv.Configured(s.profile)
}

func (s *Server) accountDesktop(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	if !validClaudeID(id) || !(r.Method == "GET" && (action == "vnc" || action == "vnc-credentials" || action == "clipboard" || action == "input") || r.Method == "POST" && (action == "start" || action == "stop" || action == "resize")) {
		http.NotFound(w, r)
		return
	}
	var size displaySize
	var scale string
	if action == "resize" {
		var ok bool
		if size, scale, ok = readDisplaySize(w, r); !ok {
			return
		}
	} else if r.Method == "POST" {
		var input struct{}
		if !readJSON(w, r, &input) {
			return
		}
	}
	if !s.accountDesktopsSupported() {
		http.Error(w, "此环境尚未支持独立 Desktop", http.StatusNotImplemented)
		return
	}
	if !s.vmRunning() {
		http.Error(w, "请先启动环境", 409)
		return
	}
	resolvedProvider, _, exists, err := s.resolveDesktopInstance(id)
	if err != nil {
		requestError(w, err)
		return
	}
	if !exists {
		http.NotFound(w, r)
		return
	}
	run := "/run/desktop-" + id
	switch action {
	case "input":
		// The private HOME is chosen from the authorized instance, never from
		// input text. Its IBus socket is outside the service's private /tmp.
		s.streamDesktopCommand(w, r, `exec env HOME="$HOME/.local/share/desktop-profiles/`+id+`/home" /usr/local/bin/cage-native-input --relay`)
		return
	case "clipboard":
		s.sharedDesktopClipboard(w, r, id)
		return
	case "vnc":
		s.streamDesktopSocket(w, r, run+"/vnc.sock")
		return
	case "vnc-credentials":
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		// A session with a settings manager renders at the density its viewer
		// names. One started by an earlier version keeps 96 DPI: its viewer
		// sizes the display in CSS pixels.
		b, err := s.guest(ctx, "systemctl is-active --quiet desktop-"+id+".service && cat "+run+`/password && { if test -S "$HOME/.local/share/desktop-profiles/`+id+`/home/.cache/cage-input/input.sock"; then printf 1; else printf 0; fi; } && if test -f `+run+`/xsettings.pid; then printf ' %s' "$(cat `+run+`/base-display-scale 2>/dev/null)"; fi`)
		reply, ok := accountDesktopCredentials(b)
		if err != nil || !ok {
			http.Error(w, "此 Desktop 尚未启动", 409)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		jsonReply(w, reply)
		return
	case "resize":
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		output, err := s.guestQuick(ctx, accountDesktopResizeScript(id, size.Width, size.Height, scale))
		if err != nil {
			http.Error(w, "Desktop 画面尺寸未能调整", 502)
			return
		}
		displayResizeReply(w, size, scale, output)
		return
	}
	s.launchRequest(w, r, "desktop-instance-"+action, func() error {
		if resolvedProvider != "" && resolvedProvider != "claude" {
			st := s.providerAccounts.forProvider(resolvedProvider)
			st.mu.Lock()
			plist, loadErr := s.loadProviderAccounts(resolvedProvider)
			var owner claudeAccount
			valid := false
			if loadErr == nil {
				if selected := plist.desktopOwner(id); selected != nil {
					owner = claudeAccount{
						ID:                      selected.ID,
						Source:                  resolvedProvider,
						ProfileID:               selected.guestID(),
						ProxyNodeID:             selected.ProxyNodeID,
						DesktopDedicatedNetwork: selected.DesktopDedicatedNetwork,
					}
					valid = true
				} else if plist.PendingDesktop == id {
					owner = claudeAccount{Source: resolvedProvider}
					valid = true
				} else if a := plist.find(id); a != nil && a.DesktopInstanceID == "" && action == "start" {
					a.DesktopInstanceID = id
					loadErr = s.saveProviderAccounts(resolvedProvider, plist)
					owner = claudeAccount{
						ID:                      a.ID,
						Source:                  resolvedProvider,
						ProfileID:               a.guestID(),
						ProxyNodeID:             a.ProxyNodeID,
						DesktopDedicatedNetwork: a.DesktopDedicatedNetwork,
					}
					valid = true
				}
			}
			st.mu.Unlock()
			if loadErr != nil {
				return loadErr
			}
			if !valid {
				return errors.New("Desktop 归属已变化，请刷新列表")
			}
			return s.changeAccountDesktop(s.operationContext(), id, owner, action)
		}
		// Reload inside the job's operation lock. A draft can be removed or
		// its dedicated-network selection can change between admission and execution.
		s.claudeAccounts.mu.Lock()
		current, err := s.loadClaudeAccounts()
		if err != nil {
			s.claudeAccounts.mu.Unlock()
			return err
		}
		selected := current.desktopOwner(id)
		// Older callers can launch an existing account by its registry ID.
		if selected == nil && current.PendingDesktop != id && current.DesktopRedirects[id] == "" {
			if a := current.find(id); a != nil && a.DesktopInstanceID == "" && action == "start" {
				a.DesktopInstanceID = id
				err = s.saveClaudeAccounts(current)
				selected = a
			}
		}
		var owner claudeAccount
		if selected != nil {
			owner = *selected
		}
		s.claudeAccounts.mu.Unlock()
		if err != nil {
			return err
		}
		if selected == nil && current.PendingDesktop != id {
			return errors.New("Desktop 归属已变化，请刷新列表")
		}
		return s.changeAccountDesktop(s.operationContext(), id, owner, action)
	})
}

func desktopProviderOf(account claudeAccount) string {
	if account.Source != "" && account.Source != "managed" && account.Source != "default" && cage.ValidProviderID(account.Source) {
		return account.Source
	}
	return "claude"
}

// accountDesktopCredentials reads the password, whether native input is
// available and, for a session that follows its viewer, a space and the
// configured density.
func accountDesktopCredentials(b []byte) (map[string]any, bool) {
	if len(b) < 9 || (b[8] != '0' && b[8] != '1') || len(b) > 9 && b[9] != ' ' {
		return nil, false
	}
	dynamic, density := len(b) > 9, 1.0
	if dynamic {
		density = 2
		if strings.TrimSpace(string(b[10:])) == "1.5" {
			density = 1.5
		}
	}
	return map[string]any{"password": string(b[:8]), "native_input": b[8] == '1', "display_scale": density, "dynamic_scale": dynamic}, true
}

// The instance's X display is in its private /tmp and, with a dedicated
// network, in its own network namespace: the mode commands run in both, as
// the workspace user. id is a validated instance ID.
func accountDesktopResizeScript(id string, wid, hei int, scale string) string {
	run := "/run/desktop-" + id
	prelude := `pid=$(systemctl show --property=MainPID --value desktop-` + id + `.service)
test "${pid:-0}" -gt 1
display=$(cat ` + run + `/display)
case $display in ''|*[!0-9]*) exit 1 ;; esac
`
	enter := `sudo -n nsenter --target "$pid" --mount --net --setuid "$(id -u)" --setgid "$(id -g)" -- /usr/bin/env DISPLAY=":$display" XAUTHORITY=` + run + `/Xauthority /bin/bash -se`
	return displayResizeScript(run, prelude, "", enter, wid, hei, scale)
}

func (s *Server) changeAccountDesktop(parent context.Context, instance string, account claudeAccount, action string) error {
	if s.desktopAction != nil {
		return s.desktopAction(parent, instance, account, action)
	}
	var err error
	provider := desktopProviderOf(account)
	scope := "stop"
	if action == "delete" {
		scope = "delete"
	}
	var key []byte
	if action == "start" {
		scope = "shared"
		if account.DesktopDedicatedNetwork {
			scope = "dedicated"
		}
		if account.ProxyNodeID != "" {
			if err = s.ensureAccountNetwork(parent, account.ID, account.ProxyNodeID); err != nil {
				return err
			}
			scope = "node-" + account.ID
		}
		key, err = s.account.desktopKey()
		if err != nil {
			return err
		}
		defer clear(key)
		provision := cage.DesktopFileSupportScript()
		if provider != "claude" {
			provision = cage.ClaudeDesktopProvisionScript()
		}
		if err = s.runDesktopScript("set -euo pipefail\n"+provision, 25*time.Second); err != nil {
			return err
		}
	}
	code := account.guestID()
	if code == "" {
		code = "default"
	}
	if action == "start" && validClaudeID(instance) && s.hostToolsServing() {
		prep := fmt.Sprintf("umask 077\nmkdir -p \"$HOME/.local/share/desktop-profiles/%s/data\"\nprintf '%%s\\n' %s > \"$HOME/.local/share/desktop-profiles/%s/provider\"", instance, shellescape.Quote(provider), instance)
		switch provider {
		case "claude":
			if code == "default" {
				prep += fmt.Sprintf("\nmkdir -p \"$HOME/.local/share/desktop-profiles/%s/code\"", instance)
			}
		case "antigravity":
			prep += fmt.Sprintf("\nmkdir -p \"$HOME/.local/share/desktop-profiles/%s/home/.gemini/config\" \"$HOME/.local/share/desktop-profiles/%s/home/.gemini/antigravity\"", instance, instance)
		case "codex":
			prep += fmt.Sprintf("\nmkdir -p \"$HOME/.local/share/desktop-profiles/%s/home/.codex\"", instance)
		}
		if prepErr := s.runDesktopScript("set -euo pipefail\n"+prep+"\n", 15*time.Second); prepErr == nil {
			s.syncHostToolsConfigNow(parent)
		}
	}
	ctx, cancel := context.WithTimeout(parent, 100*time.Second)
	defer cancel()
	envPrefix := ""
	if action == "start" && account.ProxyNodeID != "" {
		tz, lang := s.accountRegionOverrides(account.ProxyNodeID)
		if tz != "" {
			envPrefix += "CAGE_ACCOUNT_TZ=" + shellescape.Quote(tz) + " "
		}
		if lang != "" {
			envPrefix += "CAGE_ACCOUNT_LANGUAGE=" + shellescape.Quote(lang) + " "
		}
	}
	command := fmt.Sprintf("exec env %s/bin/bash -c %s -- %s %s %s", envPrefix, shellescape.Quote(linuxAccountDesktop), instance, code, scope)
	if provider != "claude" {
		command += " " + shellescape.Quote(provider)
	}
	cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", command)
	cmd.Stdin = bytes.NewReader(key)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if action == "delete" {
			return errors.New("无法清理客机内的 Desktop 实例数据，暂未删除记录，请重试")
		}
		desktopName := "Claude Desktop"
		if spec, ok := cage.LookupProvider(provider); ok {
			desktopName = spec.Desktop.Name
		}
		msg := fmt.Sprintf("Desktop 操作失败，请确认已安装 %s 并检查客机服务", desktopName)
		if bytes.Contains(output, []byte("专属网络尚未就绪")) {
			msg = "此账号的专属网络尚未就绪"
		}
		return errors.New(msg)
	}
	if action == "start" {
		s.watchDesktopLogins()
	}
	return nil
}

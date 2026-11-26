package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

type providerAccount struct {
	DesktopInstanceID       string `json:"desktop_instance_id,omitempty"`
	DesktopStatus           string `json:"desktop_status,omitempty"`
	CodeUnlinked            bool   `json:"code_unlinked,omitempty"`
	ProxyNodeID             string `json:"proxy_node_id,omitempty"`
	DesktopDedicatedNetwork bool   `json:"desktop_dedicated_network,omitempty"`
	Number                  uint32 `json:"number"`
	ID                      string `json:"id"`
	Label                   string `json:"label"`
	Source                  string `json:"source"`
	ProfileID               string `json:"profile_id,omitempty"`
	Enabled                 bool   `json:"enabled"`
	Status                  string `json:"status"`
	AuthState               string `json:"auth_state,omitempty"`
	Email                   string `json:"email,omitempty"`
	Organization            string `json:"organization,omitempty"`
	Plan                    string `json:"plan,omitempty"`
	CheckedAt               int64  `json:"checked_at,omitempty"`
}

func (a providerAccount) guestID() string {
	if a.Source == "default" {
		return "default"
	}
	if a.ProfileID != "" {
		return a.ProfileID
	}
	return a.ID
}

func (a providerAccount) subject() string {
	email := strings.ToLower(strings.TrimSpace(a.Email))
	if email == "" {
		return ""
	}
	return claudeIdentityHash(email, strings.TrimSpace(a.Organization))
}

type providerGuestSnapshot struct {
	LoggedIn      bool     `json:"logged_in"`
	Email         string   `json:"email"`
	Organization  string   `json:"organization"`
	Plan          string   `json:"plan"`
	AuthState     string   `json:"auth_state,omitempty"`
	Subject       string   `json:"subject,omitempty"`
	DesktopEmails []string `json:"desktop_emails,omitempty"`
}

func (snap providerGuestSnapshot) resolvedSubject() string {
	if validClaudeSubject(snap.Subject) {
		return snap.Subject
	}
	email := strings.ToLower(strings.TrimSpace(snap.Email))
	if !snap.LoggedIn || email == "" {
		return ""
	}
	return claudeIdentityHash(email, strings.TrimSpace(snap.Organization))
}

type providerAccountList struct {
	Provider          string            `json:"provider"`
	PendingDesktop    string            `json:"pending_desktop,omitempty"`
	DesktopRedirects  map[string]string `json:"desktop_redirects,omitempty"`
	DesktopLabels     map[string]string `json:"desktop_labels,omitempty"`
	DismissedDefaults []string          `json:"dismissed_defaults,omitempty"`
	PendingProxyNode  string            `json:"pending_proxy_node,omitempty"`
	LastNumber        uint32            `json:"last_number,omitempty"`
	Accounts          []providerAccount `json:"accounts"`
	PendingLogin      string            `json:"pending_login,omitempty"`
	Scheduling        claudeScheduling  `json:"scheduling"`
}

type providerAccountState struct {
	mu             sync.Mutex
	lastPick       string
	recent         map[string]string
	defaultChecked time.Time
	defaultReading providerGuestSnapshot
	defaultError   error
	loginNotice    string
	probe          func(context.Context, string, string) (providerGuestSnapshot, error)
}

type providerAccountManagers struct {
	mu     sync.Mutex
	states map[string]*providerAccountState
}

func (m *providerAccountManagers) forProvider(provider string) *providerAccountState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.states == nil {
		m.states = map[string]*providerAccountState{}
	}
	st := m.states[provider]
	if st == nil {
		st = &providerAccountState{recent: map[string]string{}}
		m.states[provider] = st
	}
	return st
}

func validAccountLabel(raw string) bool {
	label := strings.TrimSpace(raw)
	if label == "" || utf8.RuneCountInString(label) > 80 {
		return false
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (list *providerAccountList) find(id string) *providerAccount {
	for i := range list.Accounts {
		if list.Accounts[i].ID == id {
			return &list.Accounts[i]
		}
	}
	return nil
}

func (list *providerAccountList) bySubject(subject string) *providerAccount {
	if subject == "" {
		return nil
	}
	for i := range list.Accounts {
		if list.Accounts[i].subject() == subject {
			return &list.Accounts[i]
		}
	}
	return nil
}

func (list *providerAccountList) desktopOwner(instanceID string) *providerAccount {
	if instanceID == "" {
		return nil
	}
	for i := range list.Accounts {
		if list.Accounts[i].DesktopInstanceID == instanceID {
			return &list.Accounts[i]
		}
	}
	return nil
}

func (list *providerAccountList) assignNumbers() error {
	seen := make(map[uint32]bool, len(list.Accounts))
	for _, account := range list.Accounts {
		if account.Number == 0 {
			continue
		}
		if seen[account.Number] {
			return errors.New("账号编号重复，请检查配置文件")
		}
		seen[account.Number] = true
		list.LastNumber = max(list.LastNumber, account.Number)
	}
	for i := range list.Accounts {
		if list.Accounts[i].Number == 0 {
			if list.LastNumber == math.MaxUint32 {
				return errors.New("账号编号已用尽")
			}
			list.LastNumber++
			list.Accounts[i].Number = list.LastNumber
		}
	}
	return nil
}

func (list *providerAccountList) dismissDefault(subject string) {
	if !validClaudeSubject(subject) || slices.Contains(list.DismissedDefaults, subject) {
		return
	}
	list.DismissedDefaults = append(list.DismissedDefaults, subject)
}

func (list *providerAccountList) undismissDefault(subject string) {
	if subject == "" || len(list.DismissedDefaults) == 0 {
		return
	}
	list.DismissedDefaults = slices.DeleteFunc(list.DismissedDefaults, func(item string) bool { return item == subject })
}

func (s *Server) providerAccountsFile(provider string) string {
	return filepath.Join(s.profile, provider+"-accounts.json")
}

func (s *Server) loadProviderAccounts(provider string) (providerAccountList, error) {
	spec, ok := cage.LookupProvider(provider)
	if !ok || provider == "claude" {
		return providerAccountList{}, errors.New("不支持的账号平台")
	}
	list := providerAccountList{
		Provider:   provider,
		Accounts:   []providerAccount{},
		Scheduling: (claudeScheduling{}).defaults(),
	}
	data, err := os.ReadFile(s.providerAccountsFile(provider))
	if os.IsNotExist(err) {
		return list, nil
	}
	if err != nil || json.Unmarshal(data, &list) != nil || list.Accounts == nil {
		return list, fmt.Errorf("%s 账号文件无法读取，请检查配置目录或恢复备份", spec.Name)
	}
	list.Provider = provider
	list.Scheduling = list.Scheduling.defaults()
	if !list.Scheduling.valid() {
		return list, fmt.Errorf("%s 调度设置无效，请检查配置文件", spec.Name)
	}
	ids := map[string]bool{}
	profiles := map[string]bool{}
	usedDesktops := map[string]bool{}
	if list.PendingDesktop != "" {
		if !validClaudeID(list.PendingDesktop) {
			return list, errors.New("待登录 Desktop 记录无效")
		}
		usedDesktops[list.PendingDesktop] = true
	}
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if a.Source == "" {
			a.Source = "managed"
		}
		if (a.ProxyNodeID != "" && !validClaudeID(a.ProxyNodeID)) || !validClaudeID(a.ID) || ids[a.ID] ||
			(a.Source != "managed" && a.Source != "default") ||
			(a.Source == "default" && (a.Email == "" || a.ProfileID != "")) ||
			(a.ProfileID != "" && !validClaudeID(a.ProfileID)) {
			return list, fmt.Errorf("%s 账号文件含无效或重复记录", spec.Name)
		}
		ids[a.ID] = true
		if a.Source == "managed" {
			if profiles[a.guestID()] {
				return list, errors.New("多个账号引用了同一个独立登录目录")
			}
			profiles[a.guestID()] = true
		}
		if a.DesktopInstanceID != "" {
			if !validClaudeID(a.DesktopInstanceID) || usedDesktops[a.DesktopInstanceID] {
				return list, errors.New("多个账号引用了同一个 Desktop 实例")
			}
			usedDesktops[a.DesktopInstanceID] = true
		}
	}
	if list.PendingLogin != "" && (!validClaudeID(list.PendingLogin) || ids[list.PendingLogin] || profiles[list.PendingLogin]) {
		return list, fmt.Errorf("%s 登录草稿记录损坏", spec.Name)
	}
	err = list.assignNumbers()
	return list, err
}

func (s *Server) saveProviderAccounts(provider string, list providerAccountList) error {
	spec, ok := cage.LookupProvider(provider)
	if !ok || provider == "claude" {
		return errors.New("不支持的账号平台")
	}
	list.Provider = provider
	if err := list.assignNumbers(); err != nil {
		return err
	}
	if err := writePrivate(s.providerAccountsFile(provider), list); err != nil {
		return fmt.Errorf("%s 账号设置未保存，请检查配置目录权限和磁盘空间", spec.Name)
	}
	s.desktopCatalogRevision.Add(1)
	return nil
}

func providerSnapshotCommand(provider, guestID string) string {
	cmd := "exec /usr/local/bin/cage-provider-account " + shellescape.Quote(provider) + " snapshot " + shellescape.Quote(guestID)
	if guestID == "default" {
		cmd = `set -o pipefail; cd "$HOME" && env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 /bin/bash -lic ` +
			shellescape.Quote(`printf '\nCAGE_PROVIDER_SNAPSHOT '; `+cmd) +
			` | sed -n 's/^CAGE_PROVIDER_SNAPSHOT //p'`
	}
	return cmd
}

func (s *Server) boundProviderGuestAccount(provider, guestID, subject string) (*claudeAccount, error) {
	list, err := s.loadProviderAccounts(provider)
	if err != nil {
		return nil, err
	}
	if guestID != "" && guestID == list.PendingLogin && list.PendingProxyNode != "" {
		if !s.accountDesktopsSupported() {
			return nil, errUnsupportedAccountNetwork
		}
		return &claudeAccount{ID: guestID, ProxyNodeID: list.PendingProxyNode}, nil
	}
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if a.guestID() == guestID && a.ProxyNodeID != "" && (guestID != "default" || subject == "" || a.subject() == subject) {
			if !s.accountDesktopsSupported() {
				return nil, errUnsupportedAccountNetwork
			}
			return &claudeAccount{ID: a.ID, ProxyNodeID: a.ProxyNodeID, Email: a.Email, Organization: a.Organization}, nil
		}
	}
	return nil, nil
}

func (s *Server) probeProviderGuest(ctx context.Context, provider, guestID string) (providerGuestSnapshot, error) {
	st := s.providerAccounts.forProvider(provider)
	if st.probe != nil {
		return st.probe(ctx, provider, guestID)
	}
	var snap providerGuestSnapshot
	if !validClaudeGuestID(guestID) {
		return snap, errors.New("账号目录标识无效")
	}
	cmd := providerSnapshotCommand(provider, guestID)
	a, err := s.boundProviderGuestAccount(provider, guestID, "")
	if err != nil {
		return snap, err
	}
	if a != nil {
		if guestID == "default" {
			cmd = wrapOfflineAccountNamespace(cmd)
		} else if cmd, err = s.routeSelected(ctx, *a, cmd); err != nil {
			return snap, err
		}
	}
	data, stderr, err := s.guestControlWithStderr(ctx, cmd)
	if err != nil {
		probeErr := classifyAccountProbeError(err, stderr)
		diagnostics.Log("warn", "provider-accounts", fmt.Sprintf("provider=%s guest=%s probe failed: exit=%d err=%v stderr=%q", provider, guestID, probeErr.exitCode, err, sanitizeAccountNetworkStderr(stderr)))
		return snap, probeErr
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return snap, errors.New("无法解析账号状态，请同步环境组件后重试")
	}
	return snap, nil
}

func (s *Server) archiveProviderDefaultLogin(ctx context.Context, provider, sourceID, targetID, subject string) (providerGuestSnapshot, error) {
	var snap providerGuestSnapshot
	cmd := "exec /usr/local/bin/cage-provider-account " + shellescape.Quote(provider) + " archive-login " +
		shellescape.Quote(targetID) + " " + shellescape.Quote(sourceID) + " " + shellescape.Quote(subject)
	cmd = wrapOfflineAccountNamespace(cmd)
	data, err := s.guest(ctx, cmd)
	if err != nil || json.Unmarshal(data, &snap) != nil {
		return snap, errors.New("无法复制默认登录凭据到独立账号目录")
	}
	return snap, nil
}

func (s *Server) providerLoginActive(provider, id string) bool {
	if id == "" {
		return false
	}
	for _, t := range s.terminals.snapshot() {
		if t["closed"] == false && t["provider"] == provider && (t["provider_account_id"] == id || t["login_guest_id"] == id) && isProviderLoginKind(fmt.Sprint(t["kind"])) {
			return true
		}
	}
	return false
}

func (s *Server) providerAccountForRoute(provider, id string) (claudeAccount, error) {
	if provider == "" || provider == "claude" {
		return s.accountForRoute(id)
	}
	if id == "" {
		return claudeAccount{}, nil
	}
	st := s.providerAccounts.forProvider(provider)
	st.mu.Lock()
	defer st.mu.Unlock()
	list, err := s.loadProviderAccounts(provider)
	if err != nil {
		return claudeAccount{}, err
	}
	a := list.find(id)
	if a == nil {
		if list.PendingLogin != id {
			return claudeAccount{}, errors.New("账号记录已变化，请重新选择账号")
		}
		return claudeAccount{ID: id, Source: provider, ProxyNodeID: list.PendingProxyNode}, nil
	}
	return claudeAccount{
		ID:                      a.ID,
		Source:                  provider,
		ProfileID:               a.guestID(),
		Label:                   a.Label,
		Email:                   a.Email,
		Organization:            a.Organization,
		ProxyNodeID:             a.ProxyNodeID,
		DesktopInstanceID:       a.DesktopInstanceID,
		DesktopDedicatedNetwork: a.DesktopDedicatedNetwork,
	}, nil
}

func (s *Server) routeProviderAccountCommand(ctx context.Context, provider, id, command string) (string, error) {
	if provider == "" || provider == "claude" {
		return s.routeAccountCommand(ctx, id, command)
	}
	a, err := s.providerAccountForRoute(provider, id)
	if err != nil {
		return "", err
	}
	if a.ID != "" && a.ProxyNodeID != "" {
		command = "export CAGE_LOGIN_OWNER=" + a.ID + "; " + command
		return s.routeSelected(ctx, a, command)
	}
	if a.ID != "" && validClaudeID(a.ID) {
		command = "export CAGE_LOGIN_OWNER=" + a.ID + "; " + command
	}
	return command, nil
}

func (s *Server) setProviderLoginDraftNode(provider, id, node string) error {
	st := s.providerAccounts.forProvider(provider)
	st.mu.Lock()
	defer st.mu.Unlock()
	list, err := s.loadProviderAccounts(provider)
	if err != nil {
		return err
	}
	if id == "" || list.PendingLogin != id {
		return errors.New("新增账号授权已结束，请刷新")
	}
	if node != "" {
		if err := s.validateAccountNode(node); err != nil {
			return err
		}
	}
	if node == list.PendingProxyNode {
		return nil
	}
	if s.providerLoginActive(provider, id) {
		return errors.New("已有授权正在进行，请先完成或取消")
	}
	list.PendingProxyNode = node
	return s.saveProviderAccounts(provider, list)
}

func (s *Server) setProviderAccountNode(provider string, a *providerAccount, node string) error {
	if !s.accountDesktopsSupported() {
		return errors.New("当前环境不支持账号出口")
	}
	if node != "" {
		if err := s.validateAccountNode(node); err != nil {
			return err
		}
	}
	if node == a.ProxyNodeID && !a.DesktopDedicatedNetwork {
		return nil
	}
	for _, t := range s.terminals.snapshot() {
		if t["provider"] == provider && t["provider_account_id"] == a.ID && t["closed"] == false {
			return errors.New("请先关闭该账号的终端，再切换出口")
		}
	}
	if s.vmRunning() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if a.DesktopInstanceID != "" {
			unit := "desktop-" + a.DesktopInstanceID + ".service"
			state, err := s.guest(ctx, fmt.Sprintf("systemctl show --property=ActiveState --value %s", unit))
			if err != nil {
				return errors.New("无法确认账号 Desktop 状态，暂未更改出口")
			}
			switch strings.TrimSpace(string(state)) {
			case "inactive", "failed":
			default:
				return errors.New("请先关闭该账号的 Desktop，再切换出口")
			}
		}
		if a.ProxyNodeID != "" {
			if err := s.checkAccountNetworkIdle(ctx, a.ID, "无法确认账号进程状态，暂未更改出口", "该账号仍有后台进程，请结束后再切换出口"); err != nil {
				return err
			}
		}
	}
	oldNode := a.ProxyNodeID
	a.ProxyNodeID = node
	a.DesktopDedicatedNetwork = false
	if oldNode != "" && node == "" {
		s.stopAccountNetworkLink(a.ID)
	}
	return nil
}

func (s *Server) listProviderAccounts(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if provider == "claude" {
		s.listClaudeAccounts(w, r)
		return
	}
	spec, ok := cage.LookupProvider(provider)
	if !ok {
		http.NotFound(w, r)
		return
	}
	epoch, unlocked := s.account.access()
	if !unlocked {
		http.Error(w, errClaudeAccessLost.Error(), http.StatusLocked)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	stop := context.AfterFunc(epoch, cancel)
	defer func() { stop(); cancel() }()

	st := s.providerAccounts.forProvider(provider)
	s.mu.Lock()
	vmUp := s.vmRunning()
	updateReq := s.guestUpdateRequired
	s.mu.Unlock()

	environment := "ready"
	if !vmUp {
		environment = "stopped"
	} else if updateReq {
		environment = "update_required"
	}

	var defaultSnap providerGuestSnapshot
	st.mu.Lock()
	needDefaultProbe := environment == "ready" && time.Since(st.defaultChecked) > 20*time.Second
	if !needDefaultProbe {
		defaultSnap = st.defaultReading
	}
	st.mu.Unlock()

	if needDefaultProbe {
		snap, err := s.probeProviderGuest(ctx, provider, "default")
		st.mu.Lock()
		st.defaultChecked = time.Now()
		st.defaultError = err
		if err == nil {
			st.defaultReading = snap
			defaultSnap = snap
		}
		st.mu.Unlock()
	}

	st.mu.Lock()
	list, err := s.loadProviderAccounts(provider)
	notice := st.loginNotice
	st.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}

	// Probe any unchecked or signed_out managed accounts lightly when VM is ready.
	if environment == "ready" {
		prevChecked := make(map[string]int64, len(list.Accounts))
		changed := false
		for i := range list.Accounts {
			a := &list.Accounts[i]
			if a.CodeUnlinked || (a.CheckedAt > 0 && time.Since(time.Unix(a.CheckedAt, 0)) < 60*time.Second) {
				continue
			}
			prevChecked[a.ID] = a.CheckedAt
			snap, probeErr := s.probeProviderGuest(ctx, provider, a.guestID())
			if probeErr != nil {
				delete(prevChecked, a.ID)
				continue
			}
			a.CheckedAt = time.Now().Unix()
			a.AuthState = snap.AuthState
			if snap.LoggedIn && snap.resolvedSubject() != "" {
				if a.Email == "" || a.subject() == snap.resolvedSubject() {
					a.Email = strings.ToLower(strings.TrimSpace(snap.Email))
					a.Organization = strings.TrimSpace(snap.Organization)
					if snap.Plan != "" {
						a.Plan = snap.Plan
					}
					if a.Label == "" {
						a.Label = a.Email
					}
					a.Status = "ready"
				} else {
					a.Status = "changed"
				}
			} else if a.Status != "pending" {
				a.Status = "signed_out"
			}
			changed = true
		}
		if changed {
			st.mu.Lock()
			if current, loadErr := s.loadProviderAccounts(provider); loadErr == nil {
				wrote := false
				for _, probed := range list.Accounts {
					expectedChecked, wasProbed := prevChecked[probed.ID]
					if !wasProbed {
						continue
					}
					if target := current.find(probed.ID); target != nil && target.CheckedAt == expectedChecked && target.ProfileID == probed.ProfileID {
						target.CheckedAt = probed.CheckedAt
						target.AuthState = probed.AuthState
						target.Status = probed.Status
						if target.Email == "" && probed.Email != "" {
							target.Email = probed.Email
							target.Organization = probed.Organization
							target.Plan = probed.Plan
						}
						wrote = true
					}
				}
				if wrote {
					_ = s.saveProviderAccounts(provider, current)
				}
				list = current
			}
			st.mu.Unlock()
		}
	}

	var detectedDefault map[string]any
	defSubject := defaultSnap.resolvedSubject()
	if defaultSnap.LoggedIn && defSubject != "" && list.bySubject(defSubject) == nil && !slices.Contains(list.DismissedDefaults, defSubject) {
		detectedDefault = map[string]any{
			"identity":     defSubject,
			"email":        defaultSnap.Email,
			"organization": defaultSnap.Organization,
			"plan":         defaultSnap.Plan,
			"auth_state":   defaultSnap.AuthState,
		}
	}

	jsonReply(w, map[string]any{
		"provider":                  provider,
		"provider_name":             spec.Name,
		"cli_binary":                spec.CLI.Binary,
		"accounts":                  list.Accounts,
		"default_profile":           detectedDefault,
		"desktop_emails":            defaultSnap.DesktopEmails,
		"pending_login":             list.PendingLogin,
		"pending_proxy_node":        list.PendingProxyNode,
		"login_active":              s.providerLoginActive(provider, list.PendingLogin),
		"login_notice":              notice,
		"scheduling":                list.Scheduling,
		"desktop_network_supported": s.accountDesktopsSupported(),
		"environment":               environment,
	})
}

type providerAccountInput struct {
	Action      string            `json:"action"`
	ID          string            `json:"id"`
	Identity    string            `json:"identity"`
	Label       *string           `json:"label,omitempty"`
	Enabled     *bool             `json:"enabled,omitempty"`
	ProxyNodeID *string           `json:"proxy_node_id,omitempty"`
	Scheduling  *claudeScheduling `json:"scheduling,omitempty"`
	Order       []string          `json:"order,omitempty"`
}

func (s *Server) mutateProviderAccounts(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if provider == "claude" {
		s.mutateClaudeAccounts(w, r)
		return
	}
	spec, ok := cage.LookupProvider(provider)
	if !ok {
		http.NotFound(w, r)
		return
	}
	epoch, unlocked := s.account.access()
	if !unlocked {
		http.Error(w, errClaudeAccessLost.Error(), http.StatusLocked)
		return
	}
	var input providerAccountInput
	if !readJSON(w, r, &input) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	stop := context.AfterFunc(epoch, cancel)
	defer func() { stop(); cancel() }()

	st := s.providerAccounts.forProvider(provider)
	var err error

	switch input.Action {
	case "refresh_default":
		snap, probeErr := s.probeProviderGuest(ctx, provider, "default")
		st.mu.Lock()
		st.defaultChecked = time.Now()
		st.defaultError = probeErr
		if probeErr == nil {
			st.defaultReading = snap
		}
		st.mu.Unlock()
		err = probeErr

	case "refresh":
		if !validClaudeID(input.ID) {
			err = errors.New("账号标识无效")
			break
		}
		st.mu.Lock()
		list, loadErr := s.loadProviderAccounts(provider)
		var guestID string
		if loadErr == nil {
			if a := list.find(input.ID); a != nil {
				guestID = a.guestID()
			} else {
				loadErr = errors.New("未找到该账号")
			}
		}
		st.mu.Unlock()
		if loadErr != nil {
			err = loadErr
			break
		}
		snap, probeErr := s.probeProviderGuest(ctx, provider, guestID)
		if probeErr != nil {
			err = probeErr
			break
		}
		st.mu.Lock()
		list, err = s.loadProviderAccounts(provider)
		if err == nil {
			if a := list.find(input.ID); a != nil {
				a.CheckedAt = time.Now().Unix()
				a.AuthState = snap.AuthState
				if snap.LoggedIn && snap.resolvedSubject() != "" {
					if a.Email == "" || a.subject() == snap.resolvedSubject() {
						a.Email = strings.ToLower(strings.TrimSpace(snap.Email))
						a.Organization = strings.TrimSpace(snap.Organization)
						if snap.Plan != "" {
							a.Plan = snap.Plan
						}
						if a.Label == "" {
							a.Label = a.Email
						}
						a.Status = "ready"
						a.CodeUnlinked = false
					} else {
						a.Status = "changed"
					}
				} else {
					a.Status = "signed_out"
				}
				err = s.saveProviderAccounts(provider, list)
			}
		}
		st.mu.Unlock()

	case "join":
		if !validClaudeSubject(input.Identity) {
			err = errors.New("待加入的默认账号身份无效")
			break
		}
		if !s.vmRunning() {
			err = errors.New("请先启动 Cage 环境")
			break
		}
		targetID := randomToken()[:24]
		snap, copyErr := s.archiveProviderDefaultLogin(ctx, provider, "default", targetID, input.Identity)
		if copyErr != nil || !snap.LoggedIn || snap.resolvedSubject() != input.Identity {
			err = errors.New("无法将默认账号迁入独立配置，请使用添加账号重新登录")
			break
		}
		st.mu.Lock()
		list, loadErr := s.loadProviderAccounts(provider)
		err = loadErr
		if err == nil {
			if existing := list.bySubject(input.Identity); existing != nil {
				if existing.CodeUnlinked || existing.Status != "ready" {
					existing.Source = "managed"
					existing.ProfileID = targetID
					existing.Status = "ready"
					existing.CodeUnlinked = false
					existing.AuthState = snap.AuthState
					existing.CheckedAt = time.Now().Unix()
				}
			} else {
				list.Accounts = append(list.Accounts, providerAccount{
					ID:           targetID,
					Source:       "managed",
					ProfileID:    targetID,
					Label:        strings.ToLower(strings.TrimSpace(snap.Email)),
					Enabled:      true,
					Status:       "ready",
					AuthState:    snap.AuthState,
					Email:        strings.ToLower(strings.TrimSpace(snap.Email)),
					Organization: strings.TrimSpace(snap.Organization),
					Plan:         snap.Plan,
					CheckedAt:    time.Now().Unix(),
				})
			}
			list.undismissDefault(input.Identity)
			err = s.saveProviderAccounts(provider, list)
		}
		st.mu.Unlock()

	case "dismiss_default":
		if !validClaudeSubject(input.Identity) {
			err = errors.New("默认账号标识无效")
			break
		}
		st.mu.Lock()
		list, loadErr := s.loadProviderAccounts(provider)
		err = loadErr
		if err == nil {
			list.dismissDefault(input.Identity)
			err = s.saveProviderAccounts(provider, list)
		}
		st.mu.Unlock()

	case "update", "scheduling":
		if input.ProxyNodeID != nil {
			s.op.Lock()
			defer s.op.Unlock()
		}
		st.mu.Lock()
		list, loadErr := s.loadProviderAccounts(provider)
		err = loadErr
		if err == nil && input.Action == "scheduling" {
			if input.Scheduling != nil {
				if !input.Scheduling.valid() {
					err = errors.New("调度设置无效")
				} else {
					list.Scheduling = *input.Scheduling
				}
			}
			if err == nil && input.Order != nil {
				if len(input.Order) != len(list.Accounts) {
					err = errors.New("账号列表已变化，请刷新后排序")
				} else {
					ordered := make([]providerAccount, 0, len(input.Order))
					seen := map[string]bool{}
					for _, id := range input.Order {
						a := list.find(id)
						if a == nil || seen[id] {
							err = errors.New("账号顺序无效")
							break
						}
						seen[id] = true
						ordered = append(ordered, *a)
					}
					if err == nil {
						list.Accounts = ordered
					}
				}
			}
		} else if err == nil {
			a := list.find(input.ID)
			if a == nil {
				err = errors.New("未找到所选账号")
			} else {
				if input.Label != nil {
					trimmed := strings.TrimSpace(*input.Label)
					if trimmed == "" && a.Email != "" {
						trimmed = a.Email
					}
					if !validAccountLabel(trimmed) {
						err = errors.New("账号备注需为 1 至 80 个非控制字符")
					} else {
						a.Label = trimmed
					}
				}
				if err == nil && input.Enabled != nil {
					a.Enabled = *input.Enabled
				}
				if err == nil && input.ProxyNodeID != nil {
					err = s.setProviderAccountNode(provider, a, strings.TrimSpace(*input.ProxyNodeID))
				}
			}
		}
		if err == nil {
			err = s.saveProviderAccounts(provider, list)
		}
		st.mu.Unlock()

	case "cancel":
		if !s.op.TryLock() {
			err = errors.New("环境正在处理其他操作，请稍后重试")
			break
		}
		defer s.op.Unlock()
		st.mu.Lock()
		list, loadErr := s.loadProviderAccounts(provider)
		err = loadErr
		if err == nil {
			target := input.ID
			if target == "" {
				target = list.PendingLogin
			}
			if s.providerLoginActive(provider, target) {
				err = errors.New("请先关闭正在进行的登录终端，再取消授权")
			} else if target != "" && list.PendingLogin == target {
				if list.PendingProxyNode != "" {
					s.stopAccountNetworkLink(target)
				}
				list.PendingLogin = ""
				list.PendingProxyNode = ""
				st.loginNotice = ""
				err = s.saveProviderAccounts(provider, list)
			}
		}
		st.mu.Unlock()

	case "delete":
		if !validClaudeID(input.ID) {
			err = errors.New("未找到待删除的账号")
			break
		}
		if !s.op.TryLock() {
			err = errors.New("环境正在处理其他操作，请稍后重试")
			break
		}
		defer s.op.Unlock()
		st.mu.Lock()
		list, loadErr := s.loadProviderAccounts(provider)
		err = loadErr
		if err == nil {
			a := list.find(input.ID)
			if a != nil {
				for _, t := range s.terminals.snapshot() {
					if t["provider"] == provider && t["provider_account_id"] == a.ID && t["closed"] == false {
						err = errors.New("请先关闭该账号的终端，再删除账号")
						break
					}
				}
				if err == nil && s.accountDesktopsSupported() && s.vmRunning() {
					desktopID := a.DesktopInstanceID
					if desktopID == "" {
						desktopID = a.ID
					}
					unit := "desktop-" + desktopID + ".service"
					state, probeErr := s.guest(ctx, fmt.Sprintf("systemctl show --property=ActiveState --value %s", unit))
					if probeErr != nil {
						err = errors.New("无法确认账号 Desktop 状态，暂未删除")
					} else if trimmed := strings.TrimSpace(string(state)); trimmed != "inactive" && trimmed != "failed" {
						err = errors.New("请先关闭该账号的 Desktop，再删除账号")
					}
					if err == nil && a.ProxyNodeID != "" {
						err = s.checkAccountNetworkIdle(ctx, a.ID, "无法确认账号进程状态，暂未删除", "该账号仍有后台进程，请结束后再删除")
					}
				}
				if err == nil {
					removed := *a
					if removed.DesktopInstanceID != "" {
						delete(list.DesktopLabels, removed.DesktopInstanceID)
						for old, target := range list.DesktopRedirects {
							if old == removed.DesktopInstanceID || target == removed.DesktopInstanceID {
								delete(list.DesktopRedirects, old)
							}
						}
					}
					if sub := removed.subject(); sub != "" {
						list.dismissDefault(sub)
					}
					list.Accounts = slices.DeleteFunc(list.Accounts, func(item providerAccount) bool { return item.ID == input.ID })
					err = s.saveProviderAccounts(provider, list)
					if err == nil {
						if st.lastPick == removed.ID {
							st.lastPick = ""
						}
						for project, picked := range st.recent {
							if picked == removed.ID {
								delete(st.recent, project)
							}
						}
						if removed.ProxyNodeID != "" {
							s.stopAccountNetworkLink(removed.ID)
						}
					}
				}
			}
		}
		st.mu.Unlock()

	default:
		err = fmt.Errorf("未知 %s 账号操作", spec.Name)
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.listProviderAccounts(w, r)
}

func (s *Server) selectProviderAccount(provider, requested string, login bool, workspace string) (claudeSelection, error) {
	spec, ok := cage.LookupProvider(provider)
	if !ok || provider == "claude" {
		return claudeSelection{}, errors.New("不支持的账号平台")
	}
	st := s.providerAccounts.forProvider(provider)
	st.mu.Lock()
	defer st.mu.Unlock()
	list, err := s.loadProviderAccounts(provider)
	if err != nil {
		return claudeSelection{}, err
	}
	if login {
		st.loginNotice = ""
		if requested == "" {
			if list.PendingLogin == "" {
				id := randomToken()[:24]
				for list.find(id) != nil {
					id = randomToken()[:24]
				}
				list.PendingLogin = id
				if err := s.saveProviderAccounts(provider, list); err != nil {
					return claudeSelection{}, err
				}
			}
			return claudeSelection{ID: list.PendingLogin, GuestID: list.PendingLogin}, nil
		}
		a := list.find(requested)
		if a == nil {
			return claudeSelection{}, fmt.Errorf("未找到所选 %s 账号", spec.Name)
		}
		guestID := a.guestID()
		if guestID == "default" || a.CodeUnlinked {
			guestID = a.ID
			a.Source = "managed"
			a.ProfileID = guestID
			if err := s.saveProviderAccounts(provider, list); err != nil {
				return claudeSelection{}, err
			}
		}
		return claudeSelection{ID: a.ID, GuestID: guestID}, nil
	}

	if requested == "" || requested == "auto" {
		var eligible []providerAccount
		for _, a := range list.Accounts {
			if a.Enabled && a.Status == "ready" && !a.CodeUnlinked && a.subject() != "" {
				eligible = append(eligible, a)
			}
		}
		if len(eligible) == 0 {
			if len(list.Accounts) == 0 {
				// Allow unconfigured environment to run default slot until user adds accounts.
				return claudeSelection{GuestID: "default"}, nil
			}
			return claudeSelection{}, fmt.Errorf("当前没有已启用且完成登录的 %s 账号，请在账号管理中授权或选择“在此终端登录”", spec.Name)
		}
		var chosen providerAccount
		if list.Scheduling.Mode == "sequential" {
			chosen = eligible[0]
		} else {
			if workspace != "" {
				if preferred := st.recent[workspace]; preferred != "" {
					for _, a := range eligible {
						if a.ID == preferred {
							chosen = a
							break
						}
					}
				}
			}
			if chosen.ID == "" {
				idx := 0
				for i, a := range eligible {
					if a.ID == st.lastPick {
						idx = (i + 1) % len(eligible)
						break
					}
				}
				chosen = eligible[idx]
			}
		}
		st.lastPick = chosen.ID
		if workspace != "" {
			st.recent[workspace] = chosen.ID
		}
		return claudeSelection{ID: chosen.ID, GuestID: chosen.guestID(), Subject: chosen.subject()}, nil
	}

	a := list.find(requested)
	if a == nil {
		return claudeSelection{}, fmt.Errorf("未找到所选 %s 账号", spec.Name)
	}
	if a.Status != "ready" || a.CodeUnlinked || a.subject() == "" {
		return claudeSelection{}, fmt.Errorf("所选 %s 账号尚未完成登录", spec.Name)
	}
	st.lastPick = a.ID
	if workspace != "" {
		st.recent[workspace] = a.ID
	}
	return claudeSelection{ID: a.ID, GuestID: a.guestID(), Subject: a.subject()}, nil
}

func (s *Server) confirmProviderLogin(ctx context.Context, provider, id string) (claudeSelection, error) {
	var empty claudeSelection
	spec, ok := cage.LookupProvider(provider)
	if !ok || provider == "claude" || !validClaudeID(id) {
		return empty, errors.New("账号标识无效")
	}
	epoch, unlocked := s.account.access()
	if !unlocked {
		return empty, errClaudeAccessLost
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	stop := context.AfterFunc(epoch, cancel)
	defer func() { stop(); cancel() }()

	st := s.providerAccounts.forProvider(provider)
	st.mu.Lock()
	list, err := s.loadProviderAccounts(provider)
	guestID := id
	if err == nil {
		if a := list.find(id); a != nil && validClaudeID(a.guestID()) {
			guestID = a.guestID()
		}
	}
	st.mu.Unlock()
	if err != nil {
		return empty, err
	}

	snap, err := s.probeProviderGuest(ctx, provider, guestID)
	if err != nil {
		return empty, err
	}
	subject := snap.resolvedSubject()
	if !snap.LoggedIn || subject == "" {
		return empty, fmt.Errorf("未检测到有效的 %s 登录凭据", spec.Name)
	}

	st.mu.Lock()
	list, err = s.loadProviderAccounts(provider)
	if err != nil {
		st.mu.Unlock()
		return empty, err
	}
	now := time.Now().Unix()
	email := strings.ToLower(strings.TrimSpace(snap.Email))
	org := strings.TrimSpace(snap.Organization)
	var selected claudeSelection

	if list.PendingLogin == id {
		if dup := list.bySubject(subject); dup != nil {
			if dup.CodeUnlinked || dup.Status != "ready" {
				dup.Source = "managed"
				dup.ProfileID = guestID
				dup.Status = "ready"
				dup.CodeUnlinked = false
				dup.AuthState = snap.AuthState
				dup.CheckedAt = now
				if dup.ProxyNodeID == "" && list.PendingProxyNode != "" {
					dup.ProxyNodeID = list.PendingProxyNode
				}
				selected = claudeSelection{ID: dup.ID, GuestID: dup.guestID(), Subject: subject}
			} else {
				st.loginNotice = fmt.Sprintf("账号 %s 已在列表中，无需重复添加。", email)
				if list.PendingProxyNode != "" {
					s.stopAccountNetworkLink(id)
				}
				selected = claudeSelection{ID: dup.ID, GuestID: dup.guestID(), Subject: subject}
			}
		} else {
			list.Accounts = append(list.Accounts, providerAccount{
				ID:           id,
				Source:       "managed",
				ProfileID:    guestID,
				Label:        email,
				Enabled:      true,
				Status:       "ready",
				AuthState:    snap.AuthState,
				Email:        email,
				Organization: org,
				Plan:         snap.Plan,
				ProxyNodeID:  list.PendingProxyNode,
				CheckedAt:    now,
			})
			selected = claudeSelection{ID: id, GuestID: guestID, Subject: subject}
		}
		list.PendingLogin = ""
		list.PendingProxyNode = ""
		list.undismissDefault(subject)
	} else if a := list.find(id); a != nil {
		if dup := list.bySubject(subject); dup != nil && dup.ID != a.ID {
			st.mu.Unlock()
			return empty, fmt.Errorf("登录的账号 %s 已存在于另一条记录中", email)
		}
		a.Source = "managed"
		a.ProfileID = guestID
		a.Email = email
		a.Organization = org
		if snap.Plan != "" {
			a.Plan = snap.Plan
		}
		if a.Label == "" {
			a.Label = email
		}
		a.Status = "ready"
		a.CodeUnlinked = false
		a.AuthState = snap.AuthState
		a.CheckedAt = now
		list.undismissDefault(subject)
		selected = claudeSelection{ID: a.ID, GuestID: a.guestID(), Subject: subject}
	} else {
		// Ad-hoc terminal login (`account == "login"`) with its own fresh directory.
		if dup := list.bySubject(subject); dup != nil {
			if dup.CodeUnlinked || dup.Status != "ready" {
				dup.Source = "managed"
				dup.ProfileID = guestID
				dup.Status = "ready"
				dup.CodeUnlinked = false
				dup.AuthState = snap.AuthState
				dup.CheckedAt = now
			}
			selected = claudeSelection{ID: dup.ID, GuestID: dup.guestID(), Subject: subject}
		} else {
			list.Accounts = append(list.Accounts, providerAccount{
				ID:           guestID,
				Source:       "managed",
				ProfileID:    guestID,
				Label:        email,
				Enabled:      true,
				Status:       "ready",
				AuthState:    snap.AuthState,
				Email:        email,
				Organization: org,
				Plan:         snap.Plan,
				CheckedAt:    now,
			})
			selected = claudeSelection{ID: guestID, GuestID: guestID, Subject: subject}
		}
		list.undismissDefault(subject)
	}

	err = s.saveProviderAccounts(provider, list)
	st.mu.Unlock()
	if err != nil {
		return empty, err
	}
	s.notifications.wake()
	return selected, nil
}

func (s *Server) startProviderCodeLogin(provider string, input terminalRequest, expectedID string) (*terminalSession, error) {
	spec, ok := cage.LookupProvider(provider)
	if !ok || provider == "claude" {
		return nil, errors.New("不支持的账号平台")
	}
	epoch, unlocked := s.account.access()
	if !unlocked {
		return nil, errClaudeAccessLost
	}
	var err error
	input.Workspace, err = s.projectWorkspace(input.Workspace)
	if err != nil {
		return nil, err
	}
	ctx, done, err := s.beginActivity(epoch)
	if err != nil {
		return nil, err
	}
	draft := claudeSelection{ID: expectedID, GuestID: randomToken()[:24]}
	existingProfile := false
	if expectedID != "" {
		st := s.providerAccounts.forProvider(provider)
		st.mu.Lock()
		list, loadErr := s.loadProviderAccounts(provider)
		if loadErr == nil {
			if a := list.find(expectedID); a != nil {
				if a.Source == "managed" && !a.CodeUnlinked && a.Email != "" {
					draft.GuestID = a.guestID()
					existingProfile = true
				} else {
					draft.GuestID = a.ID
				}
			} else {
				loadErr = fmt.Errorf("未找到所选 %s 账号", spec.Name)
			}
		}
		st.mu.Unlock()
		if loadErr != nil {
			done()
			return nil, loadErr
		}
	}
	loginKind := spec.CLI.LoginKind
	loginSession, err := s.createTerminalLocked(terminalRequest{
		Kind:           loginKind,
		Workspace:      input.Workspace,
		loginDraft:     &draft,
		requestContext: input.requestContext,
	})
	if err != nil {
		done()
		return nil, err
	}
	go func() {
		defer done()
		select {
		case <-ctx.Done():
			return
		case <-loginSession.done:
		}
		if !loginSession.replaceable() {
			return
		}
		loginSession.mu.Lock()
		if existingProfile && !loginSession.exitOK {
			loginSession.loginNotice = "本次登录未完成。"
			loginSession.mu.Unlock()
			return
		}
		loginSession.loginNotice = "正在确认登录账号…"
		loginSession.mu.Unlock()
		check, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		confirmID := expectedID
		if confirmID == "" {
			confirmID = draft.GuestID
		}
		selected, err := s.confirmProviderLogin(check, provider, confirmID)
		if err == nil && selected.ID == "" {
			err = fmt.Errorf("未完成 %s 登录", spec.Name)
		}
		if err == nil && expectedID != "" && selected.ID != expectedID {
			err = errors.New("登录身份与所选账号不同，请重新选择启动账号")
		}
		if err == nil {
			s.op.Lock()
			input.AccountID = selected.ID
			input.afterLogin = loginSession
			input.requestContext = context.WithValue(ctx, accountEpochKey{}, epoch)
			if ctx.Err() != nil || !loginSession.replaceable() {
				s.op.Unlock()
				return
			}
			_, err = s.createTerminalLocked(input)
			s.op.Unlock()
			if err == nil {
				s.notifications.wake()
			}
		}
		if err != nil && ctx.Err() == nil {
			loginSession.mu.Lock()
			loginSession.loginNotice = err.Error()
			loginSession.mu.Unlock()
		}
	}()
	return loginSession, nil
}

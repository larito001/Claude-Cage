package workbench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func claudeAuthorizationURL(raw string) bool {
	if len(raw) > 8192 || strings.ContainsAny(raw, "\r\n\t \\") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery == "" {
		return false
	}
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	if path != "/oauth/authorize" && path != "/cai/oauth/authorize" {
		return false
	}
	switch strings.ToLower(u.Host) {
	case "claude.ai", "claude.com", "console.anthropic.com", "platform.claude.com", "platform.anthropic.com":
		return true
	}
	return false
}

func terminalAuthorizationURL(raw string) bool {
	if claudeAuthorizationURL(raw) {
		return true
	}
	if len(raw) > 8192 || strings.ContainsAny(raw, "\r\n\t \\") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	switch host {
	case "antigravity.google", "antigravity.google.com":
		return u.RawQuery != "" && (path == "/login" || path == "/auth" || path == "/oauth" || strings.HasPrefix(path, "/oauth/"))
	case "accounts.google.com":
		return u.RawQuery != "" && (strings.HasPrefix(path, "/o/oauth2") || path == "/AccountChooser" || strings.HasPrefix(path, "/AccountChooser/"))
	case "auth.openai.com", "auth0.openai.com":
		return u.RawQuery != "" && (path == "/authorize" || path == "/oauth/authorize" || strings.HasPrefix(path, "/u/login"))
	case "chatgpt.com", "chat.openai.com":
		return path == "/activate" || strings.HasPrefix(path, "/auth/")
	}
	return false
}

func (s *Server) terminalLoginLink(w http.ResponseWriter, r *http.Request) {
	var input struct {
		URL  string `json:"url"`
		Open bool   `json:"open,omitempty"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !terminalAuthorizationURL(input.URL) {
		http.Error(w, "请选择官方授权链接", 400)
		return
	}
	terminal := s.terminals.get(r.PathValue("id"))
	if terminal == nil {
		http.NotFound(w, r)
		return
	}
	terminal.loginLinkMu.Lock()
	defer terminal.loginLinkMu.Unlock()
	if !terminal.ownsConversation() {
		http.Error(w, "登录终端已关闭，请重新发起登录", 409)
		return
	}
	s.mu.Lock()
	ready, token := s.ready, s.credentials.Token
	s.mu.Unlock()
	if !ready {
		http.Error(w, "请先启动工作环境", 409)
		return
	}

	a, err := s.providerAccountForRoute(terminal.providerName(), terminal.anyAccountID())
	if err != nil {
		requestError(w, err)
		return
	}
	login := terminal.id
	if terminal.loginGuestID != "" {
		// Match the official CLI helper's private browser and cleanup key.
		login = terminal.loginGuestID
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	s.mu.Lock()
	reuse := s.preferences.ReuseClaudeLogin
	s.mu.Unlock()
	open := map[string]any{"login": login, "url": input.URL, "private": !reuse}
	if a.Source != "" {
		open["account"] = a.ID
	}
	if a.ProxyNodeID != "" {
		if err := s.ensureAccountNetwork(ctx, a.ID, a.ProxyNodeID); err != nil {
			requestError(w, err)
			return
		}
		open["route"] = a.ID
	}
	payload, _ := json.Marshal(open)
	data, err := s.guestAPI(ctx, token, "POST", "/login-open", payload)
	if terminal.loginGuestID == "" {
		terminal.loginLinkCleanup.Do(func() {
			go func() {
				lifetime := s.lifetimeContext()
				select {
				case <-lifetime.Done():
					return
				case <-terminal.done:
				}
				terminal.loginLinkMu.Lock()
				defer terminal.loginLinkMu.Unlock()
				ctx, cancel := context.WithTimeout(lifetime, 8*time.Second)
				defer cancel()
				body, _ := json.Marshal(map[string]string{"login": login})
				_, _ = s.guestAPI(ctx, token, "POST", "/login-finish", body)
			}()
		})
	}
	var tab struct {
		ID string `json:"id"`
	}
	if err != nil || json.Unmarshal(data, &tab) != nil || tab.ID == "" {
		http.Error(w, "Cage 浏览器未能打开授权页，请检查浏览器连接并重试", 502)
		return
	}
	terminal.mu.Lock()
	terminal.loginRequested = time.Now()
	terminal.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	jsonReply(w, map[string]any{"id": tab.ID, "incognito": !reuse})
}

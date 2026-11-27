package workbench

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/hostbridge"
)

func TestProvidersCatalogHTTP(t *testing.T) {
	s := testServer(t)
	resp := request(s, "GET", "/api/providers", s.origin, "", true)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /api/providers: %d %s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Providers []struct {
			ID     string `json:"id"`
			WebURL string `json:"web_url"`
			CLI    struct {
				Binary       string `json:"binary"`
				TerminalKind string `json:"terminal_kind"`
			} `json:"cli"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal providers: %v", err)
	}
	if len(payload.Providers) != 3 {
		t.Fatalf("expected 3 providers, got %d", len(payload.Providers))
	}
	if payload.Providers[0].ID != "claude" || payload.Providers[1].ID != "antigravity" || payload.Providers[2].ID != "codex" {
		t.Fatalf("unexpected provider order: %+v", payload.Providers)
	}
	if payload.Providers[1].CLI.Binary != "agy" || payload.Providers[2].CLI.Binary != "codex" {
		t.Fatalf("unexpected CLI binaries: %+v", payload.Providers)
	}
}

func TestBrowserSpacesIsolationAndRouteValidationHTTP(t *testing.T) {
	s := testServer(t)
	resp := request(s, "GET", "/api/browser/spaces", s.origin, "", true)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /api/browser/spaces: %d %s", resp.Code, resp.Body.String())
	}
	var view struct {
		Spaces []browserSpaceView `json:"spaces"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal spaces: %v", err)
	}
	if len(view.Spaces) != 3 {
		t.Fatalf("expected 3 browser spaces, got %d", len(view.Spaces))
	}
	if w := request(s, "POST", "/api/browser/spaces", s.origin, `{"action":"route","space":"invalid","proxy_node_id":""}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid space accepted: %d %s", w.Code, w.Body.String())
	}
	nonNilNode := strings.Repeat("d", 24)
	wNode := request(s, "POST", "/api/browser/spaces", s.origin, `{"action":"route","space":"antigravity","proxy_node_id":"`+nonNilNode+`"}`, true)
	if wNode.Code != http.StatusBadRequest && wNode.Code != http.StatusNotImplemented {
		t.Fatalf("missing/unsupported proxy node accepted: %d %s", wNode.Code, wNode.Body.String())
	}
	if w := request(s, "POST", "/api/browser/spaces", s.origin, `{"action":"route","space":"codex","proxy_node_id":""}`, true); w.Code != http.StatusOK {
		t.Fatalf("clear codex space route failed: %d %s", w.Code, w.Body.String())
	}
}

func TestMultiProviderAccountsIsolationAndScheduling(t *testing.T) {
	s := testServer(t)

	agyA := strings.Repeat("1", 24)
	agyB := strings.Repeat("2", 24)
	codexA := strings.Repeat("3", 24)

	if err := s.saveProviderAccounts("antigravity", providerAccountList{
		Accounts: []providerAccount{
			{ID: agyA, Number: 1, Label: "AGY Work", Email: "work@agy.example", Status: "ready", Enabled: true},
			{ID: agyB, Number: 2, Label: "AGY Personal", Email: "personal@agy.example", Status: "ready", Enabled: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.saveProviderAccounts("codex", providerAccountList{
		Accounts: []providerAccount{
			{ID: codexA, Number: 1, Label: "Codex Pro", Email: "pro@codex.example", Status: "ready", Enabled: true},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Verify Antigravity, Codex, and Claude registries never leak into each other.
	respAgy := request(s, "GET", "/api/provider-accounts/antigravity", s.origin, "", true)
	if respAgy.Code != http.StatusOK {
		t.Fatalf("GET antigravity accounts: %d %s", respAgy.Code, respAgy.Body.String())
	}
	var agyView struct {
		Accounts []providerAccount `json:"accounts"`
	}
	if err := json.Unmarshal(respAgy.Body.Bytes(), &agyView); err != nil || len(agyView.Accounts) != 2 {
		t.Fatalf("unexpected antigravity view: %v %+v", err, agyView)
	}

	respCodex := request(s, "GET", "/api/provider-accounts/codex", s.origin, "", true)
	if respCodex.Code != http.StatusOK {
		t.Fatalf("GET codex accounts: %d %s", respCodex.Code, respCodex.Body.String())
	}
	var codexView struct {
		Accounts []providerAccount `json:"accounts"`
	}
	if err := json.Unmarshal(respCodex.Body.Bytes(), &codexView); err != nil || len(codexView.Accounts) != 1 || codexView.Accounts[0].ID != codexA {
		t.Fatalf("unexpected codex view: %v %+v", err, codexView)
	}

	claudeList, err := s.loadClaudeAccounts()
	if err != nil || len(claudeList.Accounts) != 0 {
		t.Fatalf("provider accounts leaked into claude registry: %v %+v", err, claudeList.Accounts)
	}

	// Verify balanced scheduling alternates between ready accounts across distinct workspaces.
	firstPick, err := s.selectProviderAccount("antigravity", "auto", false, "/workspace/one")
	if err != nil || firstPick.ID != agyA {
		t.Fatalf("first auto pick: %v %+v", err, firstPick)
	}
	secondPick, err := s.selectProviderAccount("antigravity", "auto", false, "/workspace/two")
	if err != nil || secondPick.ID != agyB {
		t.Fatalf("second auto pick should balance to second account: %v %+v", err, secondPick)
	}

	// Verify rename and scheduling update over HTTP.
	updateResp := request(s, "POST", "/api/provider-accounts/antigravity", s.origin,
		`{"action":"update","id":"`+agyA+`","label":"AGY Team","enabled":false}`, true)
	if updateResp.Code != http.StatusOK {
		t.Fatalf("update antigravity account: %d %s", updateResp.Code, updateResp.Body.String())
	}
	thirdPick, err := s.selectProviderAccount("antigravity", "auto", false, "/workspace/three")
	if err != nil || thirdPick.ID != agyB {
		t.Fatalf("disabled account should not be auto-picked: %v %+v", err, thirdPick)
	}

	// Verify identity hashing lookup isolates accounts by email+organization.
	list, err := s.loadProviderAccounts("codex")
	if err != nil {
		t.Fatal(err)
	}
	sub := claudeIdentityHash("pro@codex.example", "")
	if found := list.bySubject(sub); found == nil || found.ID != codexA {
		t.Fatalf("expected subject lookup to resolve codexA, got %+v", found)
	}
}

func TestMultiProviderDesktopInstancesIsolationHTTP(t *testing.T) {
	s := testServer(t)

	claudeAcc := strings.Repeat("a", 24)
	agyAcc := strings.Repeat("b", 24)
	if err := s.saveClaudeAccounts(claudeAccountList{
		Accounts: []claudeAccount{{ID: claudeAcc, Number: 1, Label: "Claude One", Status: "ready"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.saveProviderAccounts("antigravity", providerAccountList{
		Accounts: []providerAccount{{ID: agyAcc, Number: 4, Label: "AGY Four", Status: "ready"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Create an Antigravity Desktop associated with agyAcc and an unlinked Codex Desktop.
	wAgy := request(s, "POST", "/api/desktop-instances", s.origin, `{"provider":"antigravity","account_id":"`+agyAcc+`"}`, true)
	if wAgy.Code != http.StatusOK {
		t.Fatalf("create antigravity desktop: %d %s", wAgy.Code, wAgy.Body.String())
	}
	var agyEntry desktopEntry
	if err := json.Unmarshal(wAgy.Body.Bytes(), &agyEntry); err != nil || agyEntry.Provider != "antigravity" || agyEntry.Number != 4 {
		t.Fatalf("unexpected antigravity desktop entry: %v %+v", err, agyEntry)
	}

	wCodex := request(s, "POST", "/api/desktop-instances", s.origin, `{"provider":"codex"}`, true)
	if wCodex.Code != http.StatusOK {
		t.Fatalf("create codex desktop: %d %s", wCodex.Code, wCodex.Body.String())
	}
	var codexEntry desktopEntry
	if err := json.Unmarshal(wCodex.Body.Bytes(), &codexEntry); err != nil || codexEntry.Provider != "codex" {
		t.Fatalf("unexpected codex desktop entry: %v %+v", err, codexEntry)
	}

	// Catalog keeps top-level Accounts purely Claude while exposing per-provider catalogs cleanly.
	catResp := request(s, "GET", "/api/desktop-instances", s.origin, "", true)
	if catResp.Code != http.StatusOK {
		t.Fatalf("GET /api/desktop-instances: %d %s", catResp.Code, catResp.Body.String())
	}
	var cat struct {
		Accounts  []desktopEntry `json:"accounts"`
		Providers map[string]struct {
			Accounts  []desktopEntry `json:"accounts"`
			Instances []desktopEntry `json:"instances"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(catResp.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Accounts) != 1 || cat.Accounts[0].ID != claudeAcc {
		t.Fatalf("top-level claude accounts polluted: %+v", cat.Accounts)
	}
	if len(cat.Providers["antigravity"].Instances) != 1 || cat.Providers["antigravity"].Instances[0].ID != agyEntry.ID {
		t.Fatalf("missing antigravity desktop in provider catalog: %+v", cat.Providers["antigravity"])
	}
	if len(cat.Providers["codex"].Instances) != 1 || cat.Providers["codex"].Instances[0].ID != codexEntry.ID {
		t.Fatalf("missing codex desktop in provider catalog: %+v", cat.Providers["codex"])
	}

	// Rename Codex Desktop instance; stopped VM delete returns 409, running VM delete succeeds.
	renResp := request(s, "POST", "/api/desktop-instances/"+codexEntry.ID+"/metadata", s.origin, `{"label":"Codex 独立桌面"}`, true)
	if renResp.Code != http.StatusOK {
		t.Fatalf("rename codex desktop: %d %s", renResp.Code, renResp.Body.String())
	}
	if stoppedDel := request(s, "DELETE", "/api/desktop-instances/"+codexEntry.ID, s.origin, "", true); stoppedDel.Code != http.StatusConflict {
		t.Fatalf("stopped vm delete should return 409: %d %s", stoppedDel.Code, stoppedDel.Body.String())
	}
	if err := writePrivate(filepath.Join(s.instanceDir(), "cage-running.json"), map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	s.desktopAction = func(_ context.Context, _ string, _ claudeAccount, _ string) error {
		return nil
	}
	delResp := request(s, "DELETE", "/api/desktop-instances/"+codexEntry.ID, s.origin, "", true)
	if delResp.Code != http.StatusOK {
		t.Fatalf("delete codex desktop: %d %s", delResp.Code, delResp.Body.String())
	}
}

func TestTerminalAuthorizationURLMultiProviderAndGuestScriptsLF(t *testing.T) {
	allowed := []string{
		"https://claude.ai/oauth/authorize?client_id=test&redirect_uri=http%3A%2F%2Flocalhost%3A54545%2Fcallback",
		"https://accounts.google.com/o/oauth2/v2/auth?client_id=agy&redirect_uri=http%3A%2F%2Flocalhost%3A43123%2Foauth-callback",
		"https://auth.openai.com/oauth/authorize?client_id=codex&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback",
	}
	for _, raw := range allowed {
		if !terminalAuthorizationURL(raw) {
			t.Fatalf("expected official OAuth URL %q to be recognized", raw)
		}
	}
	rejected := []string{
		"https://evil.example.com/oauth/authorize?client_id=test",
		"http://accounts.google.com/o/oauth2/v2/auth?client_id=insecure",
	}
	for _, raw := range rejected {
		if terminalAuthorizationURL(raw) {
			t.Fatalf("unexpectedly accepted untrusted URL %q", raw)
		}
	}
	for _, name := range []string{"linux-account-desktop.sh", "desktop-inspection.py", "host_tools_config.py", "provider_usage.py"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(raw), "\r") {
			t.Fatalf("embedded script %s contains CR (\\r) characters", name)
		}
	}
}

func TestMultiProviderGeneralSettingsAndNotifications(t *testing.T) {
	s := testServer(t)
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	view := settingsView(cfg, s.preferences)
	if view.ClaudeTerminalCommand != "claude" || view.AntigravityTerminalCommand != "agy" || view.CodexTerminalCommand != "codex" {
		t.Fatalf("unexpected default terminal commands: %+v", view.Preferences)
	}
	if view.ReuseClaudeLogin || view.ReuseAntigravityLogin || view.ReuseCodexLogin {
		t.Fatalf("expected incognito login default (reuse=false) for all providers: %+v", view.Preferences)
	}

	view.AntigravityTerminalCommand = "agy --model gemini-2.5-pro"
	view.CodexTerminalCommand = "codex -m o3"
	view.ReuseAntigravityLogin = true
	view.ReuseCodexLogin = false
	_, prefs, err := proposedSettings(view, cfg, s.profile)
	if err != nil {
		t.Fatalf("proposedSettings valid multi-provider settings: %v", err)
	}
	if prefs.AntigravityTerminalCommand != "agy --model gemini-2.5-pro" || prefs.CodexTerminalCommand != "codex -m o3" {
		t.Fatalf("custom commands not preserved: %+v", prefs)
	}
	if !reuseLoginForProvider(prefs, "antigravity") || reuseLoginForProvider(prefs, "codex") || reuseLoginForProvider(prefs, "claude") {
		t.Fatalf("unexpected reuseLoginForProvider result: %+v", prefs)
	}

	script := linuxLoginPreferences(prefs)
	if !strings.Contains(script, "install -m 0644 /dev/null /etc/claude-cage/login-browser-mode-antigravity") ||
		!strings.Contains(script, "unlink /etc/claude-cage/login-browser-mode-codex") {
		t.Fatalf("unexpected linuxLoginPreferences script:\n%s", script)
	}

	bad := view
	bad.CodexTerminalCommand = "codex\nwhoami"
	if _, _, err := proposedSettings(bad, cfg, s.profile); err == nil {
		t.Fatal("expected multiline CodexTerminalCommand to be rejected")
	}

	// Verify per-provider notification preferences & event provider tagging.
	w := request(s, "POST", "/api/client/notifications", s.origin,
		`{"turn_completion":"always","approval":"unfocused","sound":true,"providers":{"claude":true,"antigravity":false,"codex":true}}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/client/notifications: %d %s", w.Code, w.Body.String())
	}
	savedNotif := loadNotificationPreferences(s.notificationProfileDir())
	if savedNotif.Providers == nil || !savedNotif.Providers.Claude || savedNotif.Providers.Antigravity || !savedNotif.Providers.Codex {
		t.Fatalf("unexpected notification provider preferences: %+v", savedNotif.Providers)
	}

	eventsCh, unsub := s.notifications.subscribe()
	defer unsub()
	s.terminals.sessions = map[string]*terminalSession{
		"agyterm01": {
			id:           "agyterm01",
			title:        "Antigravity 1",
			connectionID: "conn-agy-1",
			workspace:    "/tmp/agy-demo",
			kind:         "antigravity",
			provider:     "antigravity",
		},
	}
	if !s.dispatchGuestNotification(guestChannelEvent{ID: "conn-agy-1:turn:1", Event: "turn_completed", Connection: "conn-agy-1", Seq: 1}) {
		t.Fatal("expected antigravity guest notification to publish")
	}
	ev := <-eventsCh
	if ev.Provider != "antigravity" || ev.TerminalTitle != "Antigravity 1" {
		t.Fatalf("unexpected notificationEvent: %+v", ev)
	}
}

func TestNativeProviderDesktopAppCollisionGuard(t *testing.T) {
	if !isNativeProviderDesktopApp(hostbridge.DesktopApp{Source: hostbridge.AppSource{Package: "chatgpt"}}) {
		t.Fatal("expected chatgpt package to be guarded as native provider desktop app")
	}
	if !isNativeProviderDesktopApp(hostbridge.DesktopApp{Schemes: []string{"antigravity"}}) {
		t.Fatal("expected antigravity scheme to be guarded as native provider desktop app")
	}
	if isNativeProviderDesktopApp(hostbridge.DesktopApp{
		Source:  hostbridge.AppSource{Package: "obsidian"},
		Schemes: []string{"obsidian"},
	}) {
		t.Fatal("third-party obsidian desktop app must remain allowed")
	}
}

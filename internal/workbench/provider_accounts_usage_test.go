package workbench

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderUsagePythonHelperUnitTests(t *testing.T) {
	var python string
	for _, candidate := range []string{"python3", "python"} {
		if path, err := exec.LookPath(candidate); err == nil {
			if exec.Command(path, "-I", "-c", "pass").Run() == nil {
				python = path
				break
			}
		}
	}
	if python == "" {
		t.Skip("Python unavailable")
	}
	cmd := exec.Command(python, "-I", "provider_usage_test.py")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("provider_usage_test.py: %v\n%s", err, output)
	}
	if len(providerUsageCommand("codex", "usage", strings.Repeat("a", 24), strings.Repeat("b", 64))) > 16000 {
		t.Fatal("providerUsageCommand exceeds SSH command-line budget")
	}
}

func TestProviderAccountsUsagePanelRefreshAndHeadroomScheduling(t *testing.T) {
	s := testServer(t)
	s.ready = true
	if err := writePrivate(filepath.Join(s.instanceDir(), "cage-running.json"), map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.beginClose(); s.activities.Wait() })

	codexA := strings.Repeat("1", 24)
	codexB := strings.Repeat("2", 24)
	agyA := strings.Repeat("3", 24)
	now := time.Now().Unix()

	if err := s.saveProviderAccounts("codex", providerAccountList{
		Accounts: []providerAccount{
			{ID: codexA, Number: 1, Source: "managed", ProfileID: codexA, Label: "Codex A", Email: "a@codex.example", Status: "ready", Enabled: true, CheckedAt: now},
			{ID: codexB, Number: 2, Source: "managed", ProfileID: codexB, Label: "Codex B", Email: "b@codex.example", Status: "ready", Enabled: true, CheckedAt: now},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.saveProviderAccounts("antigravity", providerAccountList{
		Accounts: []providerAccount{
			{ID: agyA, Number: 1, Source: "managed", ProfileID: agyA, Label: "AGY Ultra", Email: "ultra@agy.example", Status: "ready", Enabled: true, CheckedAt: now},
		},
	}); err != nil {
		t.Fatal(err)
	}

	codexState := s.providerAccounts.forProvider("codex")
	codexState.probe = func(_ context.Context, _ string, guestID string) (providerGuestSnapshot, error) {
		if guestID == "default" {
			return providerGuestSnapshot{LoggedIn: false}, nil
		}
		email := "a@codex.example"
		if guestID == codexB {
			email = "b@codex.example"
		}
		return providerGuestSnapshot{LoggedIn: true, Email: email, Plan: "pro", AuthState: "credentials_present"}, nil
	}
	var codexCalls atomic.Int32
	codexState.usageProbe = func(_ context.Context, provider, guestID, subject string) (providerGuestSnapshot, error) {
		codexCalls.Add(1)
		email := "a@codex.example"
		used := 100.0 // Exhausted account A!
		if guestID == codexB {
			email = "b@codex.example"
			used = 15.0
		}
		if claudeIdentityHash(email, "") != subject {
			t.Fatalf("unexpected codex subject %s for %s", subject, email)
		}
		return providerGuestSnapshot{
			LoggedIn:    true,
			Email:       email,
			Plan:        "pro",
			UsageStatus: "reported",
			Quota: &providerQuota{
				ObservedAt:   now,
				SingleWindow: "seven_day",
				SevenDay:     &claudeWindow{UsedPercentage: used, ResetsAt: now + 86400, ObservedAt: now},
			},
		}, nil
	}

	agyState := s.providerAccounts.forProvider("antigravity")
	agyState.probe = func(_ context.Context, _ string, guestID string) (providerGuestSnapshot, error) {
		if guestID == "default" {
			return providerGuestSnapshot{LoggedIn: false}, nil
		}
		return providerGuestSnapshot{LoggedIn: true, Email: "ultra@agy.example", Plan: "antigravity", AuthState: "credentials_present"}, nil
	}
	agyState.usageProbe = func(_ context.Context, provider, guestID, subject string) (providerGuestSnapshot, error) {
		return providerGuestSnapshot{
			LoggedIn:    true,
			Email:       "ultra@agy.example",
			Plan:        "Google AI Ultra",
			UsageStatus: "reported",
			Quota: &providerQuota{
				ObservedAt: now,
				FiveHour:   &claudeWindow{UsedPercentage: 3.5, ResetsAt: now + 3600, ObservedAt: now},
				SevenDay:   &claudeWindow{UsedPercentage: 14.7, ResetsAt: now + 86400, ObservedAt: now},
				Groups: []providerQuotaGroup{
					{
						ID:       "gemini",
						Label:    "Gemini",
						FiveHour: &claudeWindow{UsedPercentage: 3.5, ResetsAt: now + 3600, ObservedAt: now},
						SevenDay: &claudeWindow{UsedPercentage: 14.7, ResetsAt: now + 86400, ObservedAt: now},
					},
					{
						ID:       "partner",
						Label:    "Claude / GPT",
						FiveHour: &claudeWindow{UsedPercentage: 0, ResetsAt: 0, ObservedAt: now},
						SevenDay: &claudeWindow{UsedPercentage: 0, ResetsAt: 0, ObservedAt: now},
					},
				},
			},
		}, nil
	}

	// Passive GET must not run live usage probes.
	wGet := request(s, "GET", "/api/provider-accounts/codex?panel=visible", s.origin, "", true)
	if wGet.Code != http.StatusOK || codexCalls.Load() != 0 {
		t.Fatalf("passive GET triggered codex usage probe: code=%d calls=%d", wGet.Code, codexCalls.Load())
	}

	// Opening usage_panel checks both Codex accounts once.
	wOpen := request(s, "POST", "/api/provider-accounts/codex", s.origin, `{"action":"usage_panel"}`, true)
	if wOpen.Code != http.StatusOK {
		t.Fatalf("usage_panel codex: %d %s", wOpen.Code, wOpen.Body.String())
	}
	s.activities.Wait()
	if codexCalls.Load() != 2 {
		t.Fatalf("expected 2 codex usage probes, got %d", codexCalls.Load())
	}

	// Exhausted codexA (100% 7d) must be skipped by auto scheduling in favor of codexB (15% 7d).
	pick, err := s.selectProviderAccount("codex", "auto", false, "/workspace/demo")
	if err != nil || pick.ID != codexB {
		t.Fatalf("expected auto allocation to skip exhausted codexA and pick codexB, got %+v err=%v", pick, err)
	}

	// Manual refresh on Antigravity populates multi-group quota and updates plan to Google AI Ultra.
	wAgy := request(s, "POST", "/api/provider-accounts/antigravity", s.origin, `{"action":"refresh","id":"`+agyA+`"}`, true)
	if wAgy.Code != http.StatusOK {
		t.Fatalf("refresh antigravity: %d %s", wAgy.Code, wAgy.Body.String())
	}
	var agyView struct {
		Accounts []providerAccount `json:"accounts"`
	}
	if err := json.Unmarshal(wAgy.Body.Bytes(), &agyView); err != nil || len(agyView.Accounts) != 1 {
		t.Fatalf("unmarshal agyView: %v %+v", err, agyView)
	}
	got := agyView.Accounts[0]
	if got.Plan != "Google AI Ultra" || got.UsageStatus != "reported" || got.Quota == nil || len(got.Quota.Groups) != 2 || got.Quota.Groups[1].ID != "partner" {
		t.Fatalf("unexpected antigravity quota view: %+v quota=%+v", got, got.Quota)
	}
}

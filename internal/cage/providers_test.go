package cage

import (
	"os"
	"strings"
	"testing"
)

func TestProviderRegistryAndBrowserSpaces(t *testing.T) {
	providers := Providers()
	if len(providers) != 3 {
		t.Fatalf("expected 3 providers, got %d", len(providers))
	}
	wantOrder := []string{"claude", "antigravity", "codex"}
	for i, want := range wantOrder {
		if providers[i].ID != want {
			t.Fatalf("provider[%d] = %q, want %q", i, providers[i].ID, want)
		}
		if !ValidProviderID(want) {
			t.Fatalf("ValidProviderID(%q) = false", want)
		}
		if !ValidBrowserSpaceID(want) {
			t.Fatalf("ValidBrowserSpaceID(%q) = false", want)
		}
		spec, ok := LookupProvider(want)
		if !ok || spec.ID != want || spec.CLI.Binary == "" || spec.WebURL == "" || spec.Desktop.Binary == "" {
			t.Fatalf("LookupProvider(%q) invalid: %+v", want, spec)
		}
	}
	if ValidProviderID("unknown") || ValidBrowserSpaceID("default") {
		t.Fatal("unexpectedly accepted invalid provider/space id")
	}
}

func TestDesktopInstallScriptsExistAndUseUnixLineEndings(t *testing.T) {
	for _, id := range []string{"claude", "antigravity", "codex"} {
		script, ok := DesktopInstallScriptFor(id)
		if !ok || strings.TrimSpace(script) == "" {
			t.Fatalf("DesktopInstallScriptFor(%q) missing", id)
		}
		if strings.Contains(script, "\r") {
			t.Fatalf("DesktopInstallScriptFor(%q) contains CR (\\r) characters which break Linux bash", id)
		}
	}
	if _, ok := DesktopInstallScriptFor("unknown"); ok {
		t.Fatal("expected unknown desktop installer lookup to fail")
	}
	for _, name := range []string{
		"provider-account.py",
		"browser-spaces.mjs",
		"desktop-auth-browser.py",
		"desktop-identity.py",
		"antigravity-desktop-install.sh",
		"codex-desktop-install.sh",
	} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(raw), "\r") {
			t.Fatalf("embedded guest script %s contains CR (\\r) characters", name)
		}
	}
}

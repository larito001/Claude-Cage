package workbench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sylvinus/agent-vm/internal/hostbridge"
)

func TestSettingOptionsFromDeclaredResource(t *testing.T) {
	const uri = "notes://workspaces"
	server := mcp.NewServer(&mcp.Implementation{Name: "Notes", Version: "1"}, nil)
	server.AddResource(&mcp.Resource{URI: uri, Name: "workspaces"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, Text: `{"data":{"rows":[{"key":"b","title":"Main"},{"key":"a"},{"key":"b"}]}}`}}}, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	s := testServer(t)
	manifest := installSettingsPlugin(t, s, hostbridge.KindHost)
	dir := filepath.Join(s.hostPluginDir(), "notes")
	manifest.Permissions = []string{"network"}
	// A required selection is intentionally empty during initial discovery.
	manifest.Contributes.Settings = append(manifest.Contributes.Settings, hostbridge.Setting{ID: "workspace", Title: "Workspace", Type: "string", Env: "NOTES_WORKSPACE", Required: true})
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hostbridge.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	const declaration = `{"url":{"endpointSetting":"url","resource":"notes://workspaces"}}`
	file := filepath.Join(dir, "setting-options.json")
	if err := os.WriteFile(file, []byte(declaration), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := hostbridge.LoadManifest(dir); err == nil {
		t.Fatal("accepted a self-referencing endpoint")
	}
	if err := os.WriteFile(file, []byte(`{"workspace":{"endpointSetting":"url","tokenSetting":"token","resource":"notes://workspaces","itemsPath":["data","rows"],"valueField":"key","labelField":"title"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.savePluginSettings("notes", map[string]*string{"url": settingText(endpoint.URL), "token": settingText("test")}, false); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.getSettingOptions(w, trustedPluginRequest(s, http.MethodGet, settingOptionsPath+"?plugin=notes&setting=workspace", ""))
	var got []settingOption
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || !slices.Equal(got, []settingOption{{"b", "Main"}, {"a", "a"}}) {
		t.Fatalf("options: %d %s", w.Code, w.Body.String())
	}
}

func TestSettingOptionsRequiresTrustedView(t *testing.T) {
	response := httptest.NewRecorder()
	s := testServer(t)
	s.getSettingOptions(response, httptest.NewRequest(http.MethodGet, settingOptionsPath, nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("untrusted picker request returned %d", response.Code)
	}
}

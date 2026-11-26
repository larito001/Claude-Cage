package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sylvinus/agent-vm/internal/hostbridge"
)

const settingOptionsPath = "/api/host-tools/setting-options"

func (s *Server) getSettingOptions(w http.ResponseWriter, r *http.Request) {
	if !s.hasPluginControlView(r) {
		http.Error(w, "请在受信任的 Cage 窗口查询插件选项", http.StatusForbidden)
		return
	}
	manifest, ok := s.hostViewManifest(r.URL.Query().Get("plugin"))
	if !ok {
		http.Error(w, "请先启用插件", http.StatusConflict)
		return
	}
	settings := pluginSettingsOf(manifest)
	index := slices.IndexFunc(settings, func(field hostbridge.Setting) bool { return field.ID == r.URL.Query().Get("setting") })
	if index < 0 || settings[index].OptionsSource == nil {
		http.Error(w, "该设置未声明动态选项", http.StatusNotFound)
		return
	}
	source := settings[index].OptionsSource
	endpoint, token, err := s.settingOptionsConnection(manifest, source)
	if err != nil {
		http.Error(w, "请检查已保存的连接设置并确认 Cage 已解锁", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	options, err := listSettingOptions(ctx, endpoint, token, source)
	if err != nil {
		http.Error(w, "无法读取插件选项，请检查连接设置、选项来源和 MCP 服务状态。", http.StatusBadGateway)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonReply(w, options)
}

// Read the endpoint and its own credential together, even when other required
// settings still have no value and the plugin cannot start yet.
func (s *Server) settingOptionsConnection(m hostbridge.Manifest, source *hostbridge.SettingOptionsSource) (string, string, error) {
	s.pluginSettingsMu.Lock()
	defer s.pluginSettingsMu.Unlock()
	record := s.readPluginSettings()[m.Name]
	var endpoint, token string
	for _, setting := range pluginSettingsOf(m) {
		if setting.ID == source.EndpointSetting {
			endpoint = strings.TrimSpace(pluginSettingValue(setting, record))
		}
	}
	if endpoint == "" {
		return "", "", errors.New("missing endpoint")
	}
	if source.TokenSetting != "" && slices.Contains(record.Secrets, source.TokenSetting) {
		err := s.account.withPluginSecrets(false, func(all map[string]map[string]string) error {
			token = all[m.Name][source.TokenSetting]
			return nil
		})
		if err != nil {
			return "", "", err
		}
	}
	return endpoint, token, nil
}

type settingOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type optionsTransport struct {
	base  *http.Transport
	token string
}

func (t optionsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.token != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(r)
}

func listSettingOptions(ctx context.Context, endpoint, token string, source *hostbridge.SettingOptionsSource) ([]settingOption, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid MCP endpoint")
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := mcp.NewClient(&mcp.Implementation{Name: "Cage setting options", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: optionsTransport{transport, token}, Timeout: 8 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	result, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: source.Resource})
	if err != nil {
		return nil, err
	}
	for _, content := range result.Contents {
		if content == nil || content.URI != source.Resource || content.Text == "" {
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(content.Text), &payload); err != nil {
			return nil, err
		}
		for _, key := range source.ItemsPath {
			object, _ := payload.(map[string]any)
			payload = object[key]
		}
		items, ok := payload.([]any)
		if !ok {
			return nil, errors.New("option source must return an array")
		}
		options := make([]settingOption, 0, len(items))
		seen := map[string]bool{}
		for i, item := range items {
			value, _ := item.(string)
			object, _ := item.(map[string]any)
			if source.ValueField != "" {
				value, _ = object[source.ValueField].(string)
			}
			if value == "" || !hostbridge.ValidSettingValue(value) {
				return nil, fmt.Errorf("invalid option value at index %d", i)
			}
			if seen[value] {
				continue
			}
			label := value
			if source.LabelField != "" {
				if display, ok := object[source.LabelField].(string); ok && display != "" {
					label = display
				}
			}
			options = append(options, settingOption{value, label})
			seen[value] = true
		}
		return options, nil
	}
	return nil, errors.New("missing option resource")
}

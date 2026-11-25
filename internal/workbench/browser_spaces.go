package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

type browserSpaceConfig struct {
	RouteID     string `json:"route_id"`
	ProxyNodeID string `json:"proxy_node_id,omitempty"`
}

type browserSpacesStore struct {
	Spaces map[string]browserSpaceConfig `json:"spaces"`
}

type browserSpaceView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	WebURL      string `json:"web_url"`
	ProxyNodeID string `json:"proxy_node_id,omitempty"`
}

func (s *Server) providersCatalog(w http.ResponseWriter, _ *http.Request) {
	jsonReply(w, map[string]any{"providers": cage.Providers()})
}

func (s *Server) loadBrowserSpacesLocked() (browserSpacesStore, bool, error) {
	store := browserSpacesStore{Spaces: map[string]browserSpaceConfig{}}
	path := filepath.Join(s.profile, "browser-spaces.json")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return store, false, errors.New("无法读取浏览器空间配置")
	}
	if err == nil {
		if json.Unmarshal(data, &store) != nil {
			return store, false, errors.New("浏览器空间配置文件已损坏")
		}
		if store.Spaces == nil {
			store.Spaces = map[string]browserSpaceConfig{}
		}
	}
	usedRoutes := map[string]bool{}
	changed := false
	for key, cfg := range store.Spaces {
		if !cage.ValidBrowserSpaceID(key) || (cfg.ProxyNodeID != "" && !validClaudeID(cfg.ProxyNodeID)) {
			return store, false, errors.New("浏览器空间配置包含无效记录")
		}
		if cfg.RouteID != "" {
			if !validClaudeID(cfg.RouteID) || usedRoutes[cfg.RouteID] {
				return store, false, errors.New("浏览器空间路由标识冲突或无效")
			}
			usedRoutes[cfg.RouteID] = true
		}
	}
	for _, p := range cage.Providers() {
		cfg := store.Spaces[p.BrowserSpace]
		if cfg.RouteID == "" {
			routeID := randomToken()[:24]
			for usedRoutes[routeID] {
				routeID = randomToken()[:24]
			}
			usedRoutes[routeID] = true
			cfg.RouteID = routeID
			store.Spaces[p.BrowserSpace] = cfg
			changed = true
		}
	}
	return store, changed, nil
}

func (s *Server) saveBrowserSpacesLocked(store browserSpacesStore) error {
	if err := writePrivate(filepath.Join(s.profile, "browser-spaces.json"), store); err != nil {
		return errors.New("无法保存浏览器空间配置")
	}
	return nil
}

func (s *Server) ensureBrowserSpaceConfig(space string) (browserSpaceConfig, cage.ProviderSpec, error) {
	provider, ok := cage.LookupProvider(space)
	if !ok {
		return browserSpaceConfig{}, cage.ProviderSpec{}, browserInputError{errors.New("浏览器空间无效")}
	}
	s.browserSpacesMu.Lock()
	defer s.browserSpacesMu.Unlock()
	store, changed, err := s.loadBrowserSpacesLocked()
	if err != nil {
		return browserSpaceConfig{}, cage.ProviderSpec{}, err
	}
	if changed {
		if err := s.saveBrowserSpacesLocked(store); err != nil {
			return browserSpaceConfig{}, cage.ProviderSpec{}, err
		}
	}
	return store.Spaces[provider.BrowserSpace], provider, nil
}

func (s *Server) browserSpacesViewsLocked(store browserSpacesStore) []browserSpaceView {
	providers := cage.Providers()
	views := make([]browserSpaceView, 0, len(providers))
	for _, p := range providers {
		cfg := store.Spaces[p.BrowserSpace]
		views = append(views, browserSpaceView{
			ID:          p.BrowserSpace,
			Name:        p.Name,
			WebURL:      p.WebURL,
			ProxyNodeID: cfg.ProxyNodeID,
		})
	}
	return views
}

func (s *Server) browserSpaces(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.browserSpacesMu.Lock()
		store, changed, err := s.loadBrowserSpacesLocked()
		if err == nil && changed {
			err = s.saveBrowserSpacesLocked(store)
		}
		var views []browserSpaceView
		if err == nil {
			views = s.browserSpacesViewsLocked(store)
		}
		s.browserSpacesMu.Unlock()
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, map[string]any{"spaces": views, "dedicated_supported": s.accountDesktopsSupported()})
		return
	}
	var input struct {
		Action      string `json:"action"`
		Space       string `json:"space"`
		ProxyNodeID string `json:"proxy_node_id"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	input.Space = strings.TrimSpace(input.Space)
	input.ProxyNodeID = strings.TrimSpace(input.ProxyNodeID)
	if input.Action != "route" || !cage.ValidBrowserSpaceID(input.Space) {
		http.Error(w, "浏览器空间操作无效", http.StatusBadRequest)
		return
	}
	if !s.accountDesktopsSupported() && input.ProxyNodeID != "" {
		http.Error(w, "当前环境不支持独立浏览器空间出口", http.StatusNotImplemented)
		return
	}
	if input.ProxyNodeID != "" {
		if err := s.validateAccountNode(input.ProxyNodeID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if !s.op.TryLock() {
		http.Error(w, "环境正在执行其他操作，请稍候", http.StatusConflict)
		return
	}
	defer s.op.Unlock()

	s.browserSpacesMu.Lock()
	store, _, err := s.loadBrowserSpacesLocked()
	if err != nil {
		s.browserSpacesMu.Unlock()
		requestError(w, err)
		return
	}
	cfg := store.Spaces[input.Space]
	if cfg.ProxyNodeID == input.ProxyNodeID {
		views := s.browserSpacesViewsLocked(store)
		s.browserSpacesMu.Unlock()
		jsonReply(w, map[string]any{"spaces": views, "dedicated_supported": s.accountDesktopsSupported()})
		return
	}
	routeID := cfg.RouteID
	s.browserSpacesMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	s.mu.Lock()
	ready, token := s.ready, s.credentials.Token
	s.mu.Unlock()

	guestRoute := ""
	if input.ProxyNodeID != "" {
		guestRoute = routeID
	}
	if s.vmRunning() && ready {
		if input.ProxyNodeID != "" {
			if err := s.ensureAccountNetwork(ctx, routeID, input.ProxyNodeID); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
		}
		payload, _ := json.Marshal(map[string]string{"space": input.Space, "route": guestRoute})
		if _, err := s.guestAPI(ctx, token, http.MethodPost, "/space-route", payload); err != nil {
			if cfg.ProxyNodeID == "" && input.ProxyNodeID != "" {
				s.stopAccountNetworkLink(routeID)
			} else if cfg.ProxyNodeID != "" && cfg.ProxyNodeID != input.ProxyNodeID {
				_ = s.ensureAccountNetwork(ctx, routeID, cfg.ProxyNodeID)
			}
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if input.ProxyNodeID == "" && cfg.ProxyNodeID != "" {
			s.stopAccountNetworkLink(routeID)
		}
	}

	s.browserSpacesMu.Lock()
	store, _, err = s.loadBrowserSpacesLocked()
	if err == nil {
		updated := store.Spaces[input.Space]
		updated.RouteID = routeID
		updated.ProxyNodeID = input.ProxyNodeID
		store.Spaces[input.Space] = updated
		err = s.saveBrowserSpacesLocked(store)
	}
	var views []browserSpaceView
	if err == nil {
		views = s.browserSpacesViewsLocked(store)
	}
	s.browserSpacesMu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, map[string]any{"spaces": views, "dedicated_supported": s.accountDesktopsSupported()})
}

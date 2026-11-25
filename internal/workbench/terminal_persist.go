package workbench

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/host"
)

const persistedTerminalsFile = "terminals.json"

type persistedTerminalsData struct {
	Version   int                 `json:"version"`
	Terminals []persistedTerminal `json:"terminals"`
}

type persistedTerminal struct {
	ID                  string `json:"id"`
	Title               string `json:"title"`
	CreationOrder       int    `json:"creation_order"`
	Workspace           string `json:"workspace"`
	Kind                string `json:"kind"`
	Provider            string `json:"provider,omitempty"`
	ClaudeAccountID     string `json:"claude_account_id,omitempty"`
	ClaudeAccountSource string `json:"claude_account_source,omitempty"`
	ProviderAccountID   string `json:"provider_account_id,omitempty"`
	PhysicalProfile     string `json:"physical_profile,omitempty"`
	SharedID            string `json:"shared_id,omitempty"`
}

func isPersistableTerminalKind(kind string) bool {
	switch kind {
	case "", "shell", "claude", "antigravity", "codex":
		return true
	default:
		return false
	}
}

func validPersistedTerminalID(id string) bool {
	if len(id) != 16 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func closedTerminalDoneChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (s *Server) savePersistedTerminals() {
	if s.profile == "" || s.clientOnly || s.desktopEntry {
		return
	}
	s.persistedTerminalsMu.Lock()
	defer s.persistedTerminalsMu.Unlock()

	type snapshotEntry struct {
		order int
		item  persistedTerminal
	}
	var collected []snapshotEntry

	s.terminals.mu.Lock()
	for _, session := range s.terminals.sessions {
		session.mu.Lock()
		skip := session.dismissed || session.backgroundLogin || !isPersistableTerminalKind(session.kind) || session.workspace == ""
		var entry persistedTerminal
		order := session.creationOrder
		if !skip {
			kind := session.kind
			if kind == "" {
				kind = "shell"
			}
			entry = persistedTerminal{
				ID:                  session.id,
				Title:               session.title,
				CreationOrder:       session.creationOrder,
				Workspace:           session.workspace,
				Kind:                kind,
				Provider:            session.provider,
				ClaudeAccountID:     session.claudeAccountID,
				ClaudeAccountSource: session.claudeAccountSource,
				ProviderAccountID:   session.providerAccountID,
				PhysicalProfile:     session.physicalProfile,
				SharedID:            session.sharedID,
			}
		}
		session.mu.Unlock()
		if !skip && validPersistedTerminalID(entry.ID) {
			collected = append(collected, snapshotEntry{order: order, item: entry})
		}
	}
	s.terminals.mu.Unlock()

	sort.Slice(collected, func(i, j int) bool {
		if collected[i].order != collected[j].order {
			return collected[i].order < collected[j].order
		}
		return collected[i].item.ID < collected[j].item.ID
	})
	if len(collected) > terminalLimit {
		collected = collected[len(collected)-terminalLimit:]
	}

	items := make([]persistedTerminal, len(collected))
	for i, c := range collected {
		items[i] = c.item
	}
	path := filepath.Join(s.profile, persistedTerminalsFile)
	if err := writePrivate(path, persistedTerminalsData{Version: 1, Terminals: items}); err != nil {
		diagnostics.Log("warn", "terminals", "save terminals.json failed: "+err.Error())
	}
}

func (s *Server) allowedPersistedProjectFolders() map[string]bool {
	allowed := map[string]bool{}
	projects, err := s.projectPaths()
	if err != nil {
		return allowed
	}
	for _, p := range projects {
		allowed[p] = true
		if folders, fErr := s.projectFolders(p); fErr == nil {
			for _, f := range folders {
				allowed[f] = true
			}
		}
	}
	return allowed
}

func (s *Server) loadPersistedTerminals() {
	if s.profile == "" || s.clientOnly || s.desktopEntry {
		return
	}
	s.persistedTerminalsMu.Lock()
	defer s.persistedTerminalsMu.Unlock()

	path := filepath.Join(s.profile, persistedTerminalsFile)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return
	}
	if err := host.CheckPrivateFile(path); err != nil {
		diagnostics.Log("warn", "terminals", "check terminals.json failed: "+err.Error())
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 256<<10 {
		return
	}
	var parsed persistedTerminalsData
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Terminals) == 0 {
		return
	}

	allowedFolders := s.allowedPersistedProjectFolders()
	doneCh := closedTerminalDoneChan()

	s.terminals.mu.Lock()
	if s.terminals.sessions == nil {
		s.terminals.sessions = make(map[string]*terminalSession)
	}
	loadedCount := 0
	for _, item := range parsed.Terminals {
		if loadedCount >= terminalLimit {
			break
		}
		if !validPersistedTerminalID(item.ID) || !isPersistableTerminalKind(item.Kind) || item.Workspace == "" {
			continue
		}
		if !allowedFolders[item.Workspace] {
			continue
		}
		if _, exists := s.terminals.sessions[item.ID]; exists {
			continue
		}
		order := item.CreationOrder
		if order <= 0 {
			s.terminals.sequence++
			order = s.terminals.sequence
		} else if order > s.terminals.sequence {
			s.terminals.sequence = order
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = "终端"
		}
		session := &terminalSession{
			id:                  item.ID,
			title:               title,
			creationOrder:       order,
			workspace:           item.Workspace,
			kind:                item.Kind,
			provider:            item.Provider,
			claudeAccountID:     item.ClaudeAccountID,
			claudeAccountSource: item.ClaudeAccountSource,
			providerAccountID:   item.ProviderAccountID,
			physicalProfile:     item.PhysicalProfile,
			sharedID:            item.SharedID,
			closed:              true,
			restorable:          true,
			clients:             map[chan []byte]bool{},
			done:                doneCh,
		}
		s.terminals.sessions[item.ID] = session
		loadedCount++
	}
	s.terminals.mu.Unlock()
}

func (s *Server) removeProjectTerminals(folders []string) {
	if len(folders) == 0 {
		return
	}
	var toRemove []string
	s.terminals.mu.Lock()
	for id, session := range s.terminals.sessions {
		session.mu.Lock()
		inFolder := slices.Contains(folders, session.workspace)
		session.mu.Unlock()
		if inFolder && session.ended() {
			toRemove = append(toRemove, id)
		}
	}
	s.terminals.mu.Unlock()
	for _, id := range toRemove {
		s.terminals.remove(id)
	}
	if len(toRemove) > 0 {
		s.savePersistedTerminals()
	}
}

func (s *Server) restoreSingleTerminal(prev *terminalSession, ordinal int) (*terminalSession, error) {
	if prev == nil || !prev.isRestorable() {
		return nil, nil
	}
	prev.mu.Lock()
	req := terminalRequest{
		Workspace:       prev.workspace,
		Kind:            prev.kind,
		ClaudeAccountID: prev.claudeAccountID,
		AccountID:       prev.providerAccountID,
		sharedID:        prev.sharedID,
		afterLogin:      prev,
		restoring:       true,
		restoreOrdinal:  ordinal,
	}
	prev.mu.Unlock()

	restored, err := s.createTerminal(req)
	if err != nil {
		if errors.Is(err, errTerminalBusy) || errors.Is(err, errTerminalNotReady) || errors.Is(err, errMobileLocked) || errors.Is(err, errTerminalsClosed) {
			return nil, err
		}
		prev.mu.Lock()
		prev.restorable = false
		prev.loginNotice = "未能自动恢复此终端：" + err.Error()
		prev.mu.Unlock()
		return nil, err
	}
	return restored, nil
}

func (s *Server) restorePersistedTerminalsAsync() {
	if s.clientOnly || s.desktopEntry || s.profile == "" {
		return
	}
	go func() {
		if !s.terminalRestoreMu.TryLock() {
			return
		}
		defer s.terminalRestoreMu.Unlock()

		for attempt := 0; attempt < 10; attempt++ {
			s.mu.Lock()
			ready, closing := s.ready, s.closing
			s.mu.Unlock()
			if closing || !ready {
				return
			}
			if s.account != nil {
				if _, unlocked := s.account.access(); !unlocked {
					return
				}
			}

			s.terminals.mu.Lock()
			var pending []*terminalSession
			for _, session := range s.terminals.sessions {
				if session.isRestorable() {
					pending = append(pending, session)
				}
			}
			s.terminals.mu.Unlock()
			if len(pending) == 0 {
				return
			}
			sort.Slice(pending, func(i, j int) bool {
				return pending[i].creationOrder < pending[j].creationOrder
			})

			ordinalByGroup := map[string]int{}
			needRetry := false
			for _, prev := range pending {
				prev.mu.Lock()
				groupKey := prev.kind + "\x00" + prev.claudeAccountID + "\x00" + prev.providerAccountID + "\x00" + prev.workspace
				prev.mu.Unlock()
				ord := ordinalByGroup[groupKey]
				ordinalByGroup[groupKey] = ord + 1

				_, err := s.restoreSingleTerminal(prev, ord)
				if errors.Is(err, errTerminalBusy) {
					needRetry = true
				}
			}
			if !needRetry {
				return
			}
			select {
			case <-s.lifetimeContext().Done():
				return
			case <-time.After(600 * time.Millisecond):
			}
		}
	}()
}

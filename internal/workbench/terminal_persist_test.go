package workbench

import (
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
)

type stubPersistPTY struct{}

func (stubPersistPTY) Read([]byte) (int, error)  { return 0, io.EOF }
func (stubPersistPTY) Write(p []byte) (int, error) { return len(p), nil }
func (stubPersistPTY) Close() error              { return nil }
func (stubPersistPTY) Resize(uint16, uint16) error { return nil }
func (stubPersistPTY) Wait() error               { return nil }
func (stubPersistPTY) Kill() error               { return nil }

func TestPersistedTerminalsSurviveRestartAndRespectDismissal(t *testing.T) {
	s := testServer(t)
	projectDir := filepath.Join(t.TempDir(), "game")
	if err := writePrivate(filepath.Join(s.profile, "projects.json"), sharedProjects{
		Paths:  []string{projectDir},
		Groups: []projectGroup{{Workspace: projectDir, Name: "game", Folders: []string{projectDir}}},
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate active project CLI sessions across all 3 AI providers + shell.
	doneOpen := make(chan struct{})
	s.terminals.sequence = 4
	s.terminals.sessions = map[string]*terminalSession{
		"0123456789abcdef": {
			id:              "0123456789abcdef",
			title:           "Claude 1",
			creationOrder:   1,
			workspace:       projectDir,
			kind:            "claude",
			provider:        "claude",
			claudeAccountID: "312c433a44517705ce76393d",
			sharedID:        "a3f1e2d4-1111-4222-8333-444455556666",
			clients:         map[chan []byte]bool{},
			done:            doneOpen,
		},
		"fedcba9876543210": {
			id:                "fedcba9876543210",
			title:             "Antigravity 2",
			creationOrder:     2,
			workspace:         projectDir,
			kind:              "antigravity",
			provider:          "antigravity",
			providerAccountID: "cc247b7738f836fc8668e3a1",
			clients:           map[chan []byte]bool{},
			done:              doneOpen,
		},
		"1122334455667788": {
			id:                "1122334455667788",
			title:             "Codex 3",
			creationOrder:     3,
			workspace:         projectDir,
			kind:              "codex",
			provider:          "codex",
			providerAccountID: "06a21ad70df93a96f57d4ddd",
			clients:           map[chan []byte]bool{},
			done:              doneOpen,
		},
		"99aabbccddeeff00": {
			id:            "99aabbccddeeff00",
			title:         "codex 账号登录",
			creationOrder: 4,
			workspace:     projectDir,
			kind:          "codex-login",
			provider:      "codex",
			clients:       map[chan []byte]bool{},
			done:          doneOpen,
		},
	}
	s.savePersistedTerminals()

	// Restart into a fresh Server pointing at the same profile.
	restarted := &Server{profile: s.profile}
	restarted.loadPersistedTerminals()

	if len(restarted.terminals.sessions) != 3 {
		t.Fatalf("expected 3 non-login terminals restored, got %d", len(restarted.terminals.sessions))
	}
	if restarted.terminals.sequence < 3 {
		t.Fatalf("expected sequence >= 3, got %d", restarted.terminals.sequence)
	}

	codex := restarted.terminals.get("1122334455667788")
	if codex == nil || !codex.isRestorable() || !codex.replaceable() {
		t.Fatalf("expected codex session restorable & replaceable, got %+v", codex)
	}
	sum := codex.summary()
	if sum["title"] != "Codex 3" || sum["provider"] != "codex" || sum["provider_account_id"] != "06a21ad70df93a96f57d4ddd" || sum["restorable"] != true {
		t.Fatalf("unexpected codex summary: %+v", sum)
	}

	// Replacing a restorable session in-place preserves ID, Title, and CreationOrder.
	cmd := exec.Command("go", "version")
	replaced, err := restarted.terminals.startReplacing(
		codex,
		false,
		cmd,
		projectDir,
		"codex",
		claudeSelection{ID: "06a21ad70df93a96f57d4ddd", GuestID: "06a21ad70df93a96f57d4ddd"},
		nil,
		func() (terminalPTY, error) { return stubPersistPTY{}, nil },
	)
	if err != nil {
		t.Fatalf("startReplacing failed: %v", err)
	}
	defer restarted.terminals.close()
	if replaced.id != "1122334455667788" || replaced.title != "Codex 3" || replaced.creationOrder != 3 {
		t.Fatalf("expected in-place replacement to keep id/title/order, got id=%q title=%q order=%d", replaced.id, replaced.title, replaced.creationOrder)
	}

	// User explicitly closes Antigravity 2 -> removed from terminals.json on next restart.
	restarted.terminals.remove("fedcba9876543210")
	restarted.savePersistedTerminals()

	third := &Server{profile: s.profile}
	third.loadPersistedTerminals()
	if third.terminals.get("fedcba9876543210") != nil {
		t.Fatal("explicitly closed terminal resurrected after restart")
	}
	if third.terminals.get("0123456789abcdef") == nil || third.terminals.get("1122334455667788") == nil {
		t.Fatal("remaining project CLI terminals missing after second restart")
	}

	// Removing the project cleans up remaining persisted terminals for its folders.
	third.removeProjectTerminals([]string{projectDir})
	fourth := &Server{profile: s.profile}
	fourth.loadPersistedTerminals()
	if len(fourth.terminals.sessions) != 0 {
		t.Fatalf("expected 0 terminals after project removal, got %d", len(fourth.terminals.sessions))
	}
}

func TestManagedProviderTerminalRestoreScriptContainsResumeLogic(t *testing.T) {
	codexSpec, ok := cage.LookupProvider("codex")
	if !ok {
		t.Fatal("missing codex spec")
	}
	script := managedProviderTerminalScriptWithRestore(
		codexSpec,
		"/mnt/cage/game",
		"codex --dangerously-bypass-approvals-and-sandbox",
		"06a21ad70df93a96f57d4ddd",
		"deadbeef",
		"conn-1",
		false,
		false,
		true,
		1,
	)
	if !strings.Contains(script, "CAGE_RESTORE_ORDINAL=1") || !strings.Contains(script, "resume") || !strings.Contains(script, "state_*.sqlite") {
		t.Fatalf("unexpected codex restore script: %s", script)
	}

	agySpec, ok := cage.LookupProvider("antigravity")
	if !ok {
		t.Fatal("missing antigravity spec")
	}
	agyScript := managedProviderTerminalScriptWithRestore(
		agySpec,
		"/mnt/cage/game",
		"agy --dangerously-skip-permissions",
		"cc247b7738f836fc8668e3a1",
		"deadbeef",
		"conn-2",
		false,
		false,
		true,
		0,
	)
	if !strings.Contains(agyScript, "--resume") || !strings.Contains(agyScript, "conversation_summaries.db") {
		t.Fatalf("unexpected antigravity restore script: %s", agyScript)
	}
}

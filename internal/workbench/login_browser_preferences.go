package workbench

import (
	"context"
	"errors"
	"strings"
)

func linuxProviderLoginMarker(path string, reuse bool) string {
	if reuse {
		return "if ! test -f " + path + "; then sudo install -m 0644 /dev/null " + path + " || exit 1; fi"
	}
	return "if test -f " + path + "; then sudo unlink " + path + " || exit 1; fi"
}

// A missing file means private. Only the root-controlled opt-out markers are
// shared between the browser user and the account desktop's private HOME.
func linuxLoginPreferences(p Preferences) string {
	return strings.Join([]string{
		linuxProviderLoginMarker("/etc/claude-cage/login-browser-mode", p.ReuseClaudeLogin),
		linuxProviderLoginMarker("/etc/claude-cage/login-browser-mode-antigravity", p.ReuseAntigravityLogin),
		linuxProviderLoginMarker("/etc/claude-cage/login-browser-mode-codex", p.ReuseCodexLogin),
	}, "\n")
}

func sameLoginPreferences(a, b Preferences) bool {
	return a.ReuseClaudeLogin == b.ReuseClaudeLogin &&
		a.ReuseAntigravityLogin == b.ReuseAntigravityLogin &&
		a.ReuseCodexLogin == b.ReuseCodexLogin
}

func (s *Server) syncLoginPreferences(ctx context.Context, p Preferences) error {
	script := linuxLoginPreferences(p)
	if _, err := s.guest(ctx, script); err != nil {
		return errors.New("未能将登录浏览器设置同步到环境，请重新连接后重试")
	}
	return nil
}

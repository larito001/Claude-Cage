package cage

import "regexp"

// ProviderSpec defines an AI platform supported inside Cage across Browser
// Spaces, isolated CLI profiles, and isolated multi-instance Desktop apps.
// Adding a new platform only requires registering a ProviderSpec entry here
// and supplying its guest runtime adapter / desktop installer if applicable.
type ProviderSpec struct {
	ID            string              `json:"id"`
	Name          string              `json:"name"`
	WebURL        string              `json:"web_url"`
	CLI           ProviderCLISpec     `json:"cli"`
	Desktop       ProviderDesktopSpec `json:"desktop"`
	BrowserSpace  string              `json:"browser_space"`
	SupportsQuota bool                `json:"supports_quota,omitempty"`
}

type ProviderCLISpec struct {
	Binary       string `json:"binary"`
	ToolName     string `json:"tool_name"`
	TerminalKind string `json:"terminal_kind"`
	LoginKind    string `json:"login_kind"`
}

type ProviderDesktopSpec struct {
	Name      string   `json:"name"`
	Package   string   `json:"package"`
	Binary    string   `json:"binary"`
	DesktopID string   `json:"desktop_id"`
	Schemes   []string `json:"schemes"`
}

var providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

var providerRegistry = []ProviderSpec{
	{
		ID:           "claude",
		Name:         "Claude",
		WebURL:       "https://claude.ai",
		BrowserSpace: "claude",
		CLI: ProviderCLISpec{
			Binary:       "claude",
			ToolName:     "claude",
			TerminalKind: "claude",
			LoginKind:    "claude-login",
		},
		Desktop: ProviderDesktopSpec{
			Name:      "Claude Desktop",
			Package:   "claude-desktop",
			Binary:    "/usr/bin/claude-desktop",
			DesktopID: "com.anthropic.Claude",
			Schemes:   []string{"claude"},
		},
		SupportsQuota: true,
	},
	{
		ID:           "antigravity",
		Name:         "Antigravity",
		WebURL:       "https://antigravity.google",
		BrowserSpace: "antigravity",
		CLI: ProviderCLISpec{
			Binary:       "agy",
			ToolName:     "agy",
			TerminalKind: "antigravity",
			LoginKind:    "antigravity-login",
		},
		Desktop: ProviderDesktopSpec{
			Name:      "Antigravity Desktop",
			Package:   "antigravity",
			Binary:    "/usr/bin/antigravity",
			DesktopID: "com.google.Antigravity",
			Schemes:   []string{"antigravity"},
		},
	},
	{
		ID:           "codex",
		Name:         "Codex",
		WebURL:       "https://chatgpt.com",
		BrowserSpace: "codex",
		CLI: ProviderCLISpec{
			Binary:       "codex",
			ToolName:     "codex",
			TerminalKind: "codex",
			LoginKind:    "codex-login",
		},
		Desktop: ProviderDesktopSpec{
			Name:      "Codex Desktop",
			Package:   "chatgpt",
			Binary:    "/usr/bin/chatgpt",
			DesktopID: "com.openai.ChatGPT",
			Schemes:   []string{"chatgpt", "codex", "openai"},
		},
	},
}

// Providers returns a defensive copy of all registered AI providers in display order.
func Providers() []ProviderSpec {
	out := make([]ProviderSpec, len(providerRegistry))
	for i, p := range providerRegistry {
		p.Desktop.Schemes = append([]string(nil), p.Desktop.Schemes...)
		out[i] = p
	}
	return out
}

// LookupProvider resolves a registered provider by ID.
func LookupProvider(id string) (ProviderSpec, bool) {
	for _, p := range providerRegistry {
		if p.ID == id {
			p.Desktop.Schemes = append([]string(nil), p.Desktop.Schemes...)
			return p, true
		}
	}
	return ProviderSpec{}, false
}

// ValidProviderID reports whether id names a registered provider.
func ValidProviderID(id string) bool {
	if !providerIDPattern.MatchString(id) {
		return false
	}
	_, ok := LookupProvider(id)
	return ok
}

// ValidBrowserSpaceID reports whether id is a registered provider browser space.
func ValidBrowserSpaceID(id string) bool {
	for _, p := range providerRegistry {
		if p.BrowserSpace == id {
			return true
		}
	}
	return false
}

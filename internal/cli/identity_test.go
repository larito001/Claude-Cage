package cli

import (
	"bytes"
	"strings"
	"testing"
)

// A program that embeds the shared commands names itself and says how it
// updates: SuperCage must not send its users to agent-vm's installer.
func TestEmbeddingProgramIdentity(t *testing.T) {
	savedCommand, savedHint := Command, UpdateHint
	t.Cleanup(func() { Command, UpdateHint = savedCommand, savedHint })
	Command, UpdateHint = "super-cage", "update SuperCage with its installer"

	var stdout, stderr bytes.Buffer
	io := IO{Stdout: &stdout, Stderr: &stderr}
	if code := versionCmd([]string{"--min", "99.0.0"}, io); code != 1 {
		t.Fatalf("an unmet minimum returned %d", code)
	}
	got := stderr.String()
	for _, want := range []string{"Error: super-cage ", "Update it:  update SuperCage with its installer"} {
		if !strings.Contains(got, want) {
			t.Errorf("the version error lacks %q: %q", want, got)
		}
	}
	if strings.Contains(got, "agent-vm") {
		t.Errorf("the version error names agent-vm: %q", got)
	}

	stderr.Reset()
	if code := licensesCmd([]string{"x"}, io); code != 2 || !strings.Contains(stderr.String(), "Usage: super-cage licenses") {
		t.Errorf("licenses usage: %d %q", code, stderr.String())
	}
}

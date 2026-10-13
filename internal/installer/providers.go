// Package installer manages AID skills installed into local agent clients.
package installer

import (
	"fmt"
	"os"
	"path/filepath"
)

type provider struct {
	ID, Name, Root string
	Detected       bool
	Installed      bool
}

func providers() ([]provider, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory: %w", err)
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(home, ".config")
	}
	definitions := []struct{ id, name, env, base, command string }{
		{"codex", "Codex", "CODEX_HOME", filepath.Join(home, ".codex"), "codex"},
		{"claude", "Claude Code", "CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"), "claude"},
		{"cursor", "Cursor", "", filepath.Join(home, ".cursor"), "cursor"},
		{"gemini", "Gemini CLI", "", filepath.Join(home, ".gemini"), "gemini"},
		{"antigravity", "Antigravity", "", filepath.Join(home, ".gemini", "antigravity-cli"), "agy"},
		{"opencode", "OpenCode", "", filepath.Join(config, "opencode"), "opencode"},
		{"copilot", "GitHub Copilot", "COPILOT_HOME", filepath.Join(home, ".copilot"), "copilot"},
	}
	result := make([]provider, 0, len(definitions))
	for _, d := range definitions {
		base := d.base
		if override := os.Getenv(d.env); override != "" {
			base = override
		}
		if !filepath.IsAbs(base) {
			return nil, fmt.Errorf("%s client directory must be absolute: %s", d.id, base)
		}
		info, statErr := os.Stat(base)
		result = append(result, provider{
			ID: d.id, Name: d.name, Root: filepath.Join(base, "skills"),
			Detected: statErr == nil && info.IsDir(),
		})
	}
	return result, nil
}

func statePath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("XDG_STATE_HOME must be absolute: %s", base)
	}
	return filepath.Join(base, "AID", "installer.json"), nil
}

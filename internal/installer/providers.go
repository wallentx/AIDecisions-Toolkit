// Package installer manages AID skills and MCP server registrations across local agent clients.
package installer

import (
	"fmt"
	"os"
	"path/filepath"
)

type provider struct {
	ID, Name, Root string
	MCPPath        string
	MCPFormat      mcpFormat
	MCPTopKey      string
	Detected       bool
	Installed      bool
}

func providers() ([]provider, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory: %w", err)
	}
	if !filepath.IsAbs(home) {
		return nil, fmt.Errorf("home directory must be absolute: %s", home)
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(config) {
		return nil, fmt.Errorf("XDG_CONFIG_HOME must be absolute: %s", config)
	}

	definitions := []struct {
		id, name, env, base string
		mcpRelFromHome      string
		mcpRelFromBase      string
		mcpFmt              mcpFormat
		mcpTopKey           string
		extraDetect         string
	}{
		{
			id: "codex", name: "Codex", env: "CODEX_HOME",
			base:           filepath.Join(home, ".codex"),
			mcpRelFromBase: "config.toml",
			mcpFmt:         mcpCodexTOML,
		},
		{
			id: "claude", name: "Claude Code", env: "CLAUDE_CONFIG_DIR",
			base:           filepath.Join(home, ".claude"),
			mcpRelFromHome: ".claude.json",
			mcpFmt:         mcpJSON,
			mcpTopKey:      "mcpServers",
		},
		{
			id: "cursor", name: "Cursor",
			base:           filepath.Join(home, ".cursor"),
			mcpRelFromBase: "mcp.json",
			mcpFmt:         mcpJSON,
			mcpTopKey:      "mcpServers",
		},
		{
			id: "gemini", name: "Gemini CLI",
			base:           filepath.Join(home, ".gemini"),
			mcpRelFromBase: "settings.json",
			mcpFmt:         mcpJSON,
			mcpTopKey:      "mcpServers",
		},
		{
			id: "antigravity", name: "Antigravity",
			base:           filepath.Join(home, ".gemini", "antigravity-cli"),
			mcpRelFromHome: filepath.Join(".gemini", "config", "mcp_config.json"),
			mcpFmt:         mcpJSON,
			mcpTopKey:      "mcpServers",
			extraDetect:    filepath.Join(home, ".gemini", "config"),
		},
		{
			id: "opencode", name: "OpenCode",
			base:           filepath.Join(config, "opencode"),
			mcpRelFromBase: "opencode.json",
			mcpFmt:         mcpOpenCodeJSON,
			mcpTopKey:      "mcp",
		},
		{
			id: "copilot", name: "GitHub Copilot", env: "COPILOT_HOME",
			base:           filepath.Join(home, ".copilot"),
			mcpRelFromBase: "mcp-config.json",
			mcpFmt:         mcpJSON,
			mcpTopKey:      "mcpServers",
		},
	}

	result := make([]provider, 0, len(definitions))
	for _, d := range definitions {
		base := d.base
		overridden := false
		if d.env != "" {
			if override := os.Getenv(d.env); override != "" {
				base = override
				overridden = true
			}
		}
		if !filepath.IsAbs(base) {
			return nil, fmt.Errorf("%s client directory must be absolute: %s", d.id, base)
		}
		var mcpPath string
		switch {
		case d.id == "claude" && overridden:
			mcpPath = filepath.Join(base, ".claude.json")
		case d.mcpRelFromBase != "":
			mcpPath = filepath.Join(base, d.mcpRelFromBase)
		case d.mcpRelFromHome != "":
			mcpPath = filepath.Join(home, d.mcpRelFromHome)
		}

		detected := pathExists(base) || (d.extraDetect != "" && pathExists(d.extraDetect)) || (d.id == "claude" && pathExists(mcpPath))
		result = append(result, provider{
			ID:        d.id,
			Name:      d.name,
			Root:      filepath.Join(base, "skills"),
			MCPPath:   mcpPath,
			MCPFormat: d.mcpFmt,
			MCPTopKey: d.mcpTopKey,
			Detected:  detected,
		})
	}
	return result, nil
}

func pathExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
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

package installer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aid/internal/catalog"
)

func TestProvidersPopulateMCPTargetsForAllSevenClients(t *testing.T) {
	home := isolateHome(t)
	ps, err := providers()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		path   string
		format mcpFormat
		topKey string
	}{
		"codex":       {filepath.Join(home, ".codex", "config.toml"), mcpCodexTOML, ""},
		"claude":      {filepath.Join(home, ".claude.json"), mcpJSON, "mcpServers"},
		"cursor":      {filepath.Join(home, ".cursor", "mcp.json"), mcpJSON, "mcpServers"},
		"gemini":      {filepath.Join(home, ".gemini", "settings.json"), mcpJSON, "mcpServers"},
		"antigravity": {filepath.Join(home, ".gemini", "config", "mcp_config.json"), mcpJSON, "mcpServers"},
		"opencode":    {filepath.Join(home, "config", "opencode", "opencode.json"), mcpOpenCodeJSON, "mcp"},
		"copilot":     {filepath.Join(home, ".copilot", "mcp-config.json"), mcpJSON, "mcpServers"},
	}
	if len(ps) != len(want) {
		t.Fatalf("got %d providers, want %d", len(ps), len(want))
	}
	for _, p := range ps {
		spec, ok := want[p.ID]
		if !ok {
			t.Fatalf("unexpected provider %q", p.ID)
		}
		if p.MCPPath != spec.path || p.MCPFormat != spec.format || p.MCPTopKey != spec.topKey {
			t.Fatalf("provider %s MCP = (%q, %q, %q), want (%q, %q, %q)", p.ID, p.MCPPath, p.MCPFormat, p.MCPTopKey, spec.path, spec.format, spec.topKey)
		}
	}
}

func TestJSONMCPInstallUpdatePreserveForeignDataAndRemove(t *testing.T) {
	home := isolateHome(t)
	ps, err := providers()
	if err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(home, "state", "AID", "installer.json")
	bin1 := filepath.Join(home, "bin1")
	bin2 := filepath.Join(home, "bin2")
	tools := []catalog.Tool{{ID: "diff", Command: "AID.diff", Skill: []byte("# Diff\n{{AID_COMMAND}}\n")}}

	claudeConfig := filepath.Join(home, ".claude.json")
	// Include a 64-bit integer > 2^53 and a foreign MCP server to verify lossless preservation.
	initialJSON := `{
  "accountId": 9007199254740993,
  "mcpServers": {
    "panoptes": {
      "command": "/usr/local/bin/panoptes",
      "args": ["mcp"]
    }
  }
}
`
	put(t, claudeConfig, []byte(initialJSON))
	if runtime.GOOS != "windows" {
		if err := os.Chmod(claudeConfig, 0644); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Install Claude + OpenCode.
	p, err := makePlan(ps, tools, map[string]bool{"claude": true, "opencode": true}, stateFile, bin1)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, p)

	claudeAfter := string(read(t, claudeConfig))
	if !strings.Contains(claudeAfter, "9007199254740993") || !strings.Contains(claudeAfter, `"panoptes"`) {
		t.Fatalf("foreign keys or large integer lost in .claude.json:\n%s", claudeAfter)
	}
	if !strings.Contains(claudeAfter, quoteJSON(launcherExecutable(bin1))) || !strings.Contains(claudeAfter, `"--mcp"`) {
		t.Fatalf("aid MCP server missing from .claude.json:\n%s", claudeAfter)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(claudeConfig)
		if err != nil || info.Mode().Perm() != 0644 {
			t.Fatalf("existing file mode changed: %v, %v", info.Mode().Perm(), err)
		}
	}

	openCodeConfig := filepath.Join(home, "config", "opencode", "opencode.json")
	var openCodeDoc struct {
		MCP map[string]struct {
			Type    string   `json:"type"`
			Command []string `json:"command"`
			Enabled bool     `json:"enabled"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(read(t, openCodeConfig), &openCodeDoc); err != nil {
		t.Fatal(err)
	}
	aidOC := openCodeDoc.MCP["aid"]
	if aidOC.Type != "local" || !aidOC.Enabled || len(aidOC.Command) != 2 || aidOC.Command[0] != launcherExecutable(bin1) || aidOC.Command[1] != "--mcp" {
		t.Fatalf("unexpected OpenCode MCP entry: %+v", aidOC)
	}

	// 2. Simulate Claude Code adding another top-level field after AID --init ran,
	// then update AID executable directory from bin1 -> bin2.
	var mutated map[string]json.RawMessage
	if err := json.Unmarshal([]byte(claudeAfter), &mutated); err != nil {
		t.Fatal(err)
	}
	mutated["numStartups"] = json.RawMessage(`42`)
	mutatedBytes, _ := json.MarshalIndent(mutated, "", "  ")
	put(t, claudeConfig, append(mutatedBytes, '\n'))

	pUpdate, err := makePlan(ps, tools, map[string]bool{"claude": true}, stateFile, bin2)
	if err != nil {
		t.Fatalf("unrelated client edits blocked MCP update: %v", err)
	}
	apply(t, pUpdate)
	claudeUpdated := string(read(t, claudeConfig))
	if !strings.Contains(claudeUpdated, `"numStartups": 42`) || !strings.Contains(claudeUpdated, quoteJSON(launcherExecutable(bin2))) || strings.Contains(claudeUpdated, quoteJSON(launcherExecutable(bin1))) {
		t.Fatalf("MCP binary path not updated cleanly:\n%s", claudeUpdated)
	}

	// 3. Remove Claude while leaving OpenCode installed; panoptes and numStartups must remain.
	pRemove, err := makePlan(ps, tools, map[string]bool{"claude": false}, stateFile, bin2)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, pRemove)
	claudeRemoved := string(read(t, claudeConfig))
	if strings.Contains(claudeRemoved, `"aid"`) || !strings.Contains(claudeRemoved, `"panoptes"`) || !strings.Contains(claudeRemoved, `"numStartups": 42`) {
		t.Fatalf("removal altered foreign data or left aid behind:\n%s", claudeRemoved)
	}
}

func TestCodexTOMLMCPInstallUpdateAndRemovePreservesForeignSections(t *testing.T) {
	home := isolateHome(t)
	ps, err := providers()
	if err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(home, "state", "AID", "installer.json")
	bin1 := filepath.Join(home, "bin1")
	bin2 := filepath.Join(home, "bin2")
	tools := []catalog.Tool{{ID: "diff", Command: "AID.diff", Skill: []byte("# Diff\n")}}

	codexTOML := filepath.Join(home, ".codex", "config.toml")
	originalTOML := `# User Codex configuration
model = "gpt-5.6-sol"

[mcp_servers.panoptes]
command = "/home/tester/.cargo/bin/panoptes" # keep comment with [brackets]
args = ["mcp"]

[projects."/home/tester/src/repo#1"]
trust_level = "trusted"
`
	put(t, codexTOML, []byte(originalTOML))

	// Install Codex.
	p, err := makePlan(ps, tools, map[string]bool{"codex": true}, stateFile, bin1)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, p)
	installed := string(read(t, codexTOML))
	if !strings.Contains(installed, "[mcp_servers.aid]") || !strings.Contains(installed, quoteJSON(launcherExecutable(bin1))) {
		t.Fatalf("missing [mcp_servers.aid]:\n%s", installed)
	}
	if !strings.Contains(installed, "[mcp_servers.panoptes]") || !strings.Contains(installed, `[projects."/home/tester/src/repo#1"]`) {
		t.Fatalf("lost foreign TOML sections:\n%s", installed)
	}

	// Update Codex to bin2.
	p2, err := makePlan(ps, tools, map[string]bool{"codex": true}, stateFile, bin2)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, p2)
	updated := string(read(t, codexTOML))
	if !strings.Contains(updated, quoteJSON(launcherExecutable(bin2))) || strings.Contains(updated, quoteJSON(launcherExecutable(bin1))) {
		t.Fatalf("failed to update [mcp_servers.aid] in place:\n%s", updated)
	}

	// Remove Codex and verify foreign TOML remains intact.
	p3, err := makePlan(ps, tools, map[string]bool{"codex": false}, stateFile, bin2)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, p3)
	removed := string(read(t, codexTOML))
	if strings.Contains(removed, "[mcp_servers.aid]") {
		t.Fatalf("[mcp_servers.aid] was not removed:\n%s", removed)
	}
	if !bytes.Equal([]byte(removed), []byte(originalTOML)) {
		t.Fatalf("TOML round-trip altered unrelated content:\ngot:\n%s\nwant:\n%s", removed, originalTOML)
	}
}

func TestMCPRefusesUnmanagedConflictAndUserEditedManagedEntry(t *testing.T) {
	home := isolateHome(t)
	ps, err := providers()
	if err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(home, "state", "AID", "installer.json")
	bin1 := filepath.Join(home, "bin1")
	bin2 := filepath.Join(home, "bin2")
	tools := []catalog.Tool{{ID: "diff", Command: "AID.diff", Skill: []byte("# Diff\n")}}

	// 1. Unmanaged conflicting "aid" server in Cursor mcp.json must be refused.
	cursorMCP := filepath.Join(home, ".cursor", "mcp.json")
	put(t, cursorMCP, []byte(`{"mcpServers":{"aid":{"command":"/other/aid-server","args":[]}}}`))
	if _, err := makePlan(ps, tools, map[string]bool{"cursor": true}, stateFile, bin1); err == nil || !strings.Contains(err.Error(), "unmanaged MCP server") {
		t.Fatalf("expected unmanaged MCP conflict error, got %v", err)
	}

	// 2. Identical unmanaged "aid" server IS adopted cleanly.
	put(t, cursorMCP, []byte(`{"mcpServers":{"aid":{"args":["--mcp"],"command":`+quoteJSON(launcherExecutable(bin1))+`}}}`))
	adoptPlan, err := makePlan(ps, tools, map[string]bool{"cursor": true}, stateFile, bin1)
	if err != nil {
		t.Fatalf("identical unmanaged MCP entry rejected: %v", err)
	}
	apply(t, adoptPlan)

	// 3. User customizes managed "aid" entry (e.g. adds custom env); update & remove must refuse to clobber it.
	put(t, cursorMCP, []byte(`{"mcpServers":{"aid":{"command":`+quoteJSON(launcherExecutable(bin1))+`,"args":["--mcp"],"env":{"AID_CONFIG":"/custom.yml"}}}}`))
	if _, err := makePlan(ps, tools, map[string]bool{"cursor": true}, stateFile, bin2); err == nil || !strings.Contains(err.Error(), "was edited") {
		t.Fatalf("expected edited MCP error on update, got %v", err)
	}
	if _, err := makePlan(ps, tools, map[string]bool{"cursor": false}, stateFile, bin1); err == nil || !strings.Contains(err.Error(), "was edited") {
		t.Fatalf("expected edited MCP error on remove, got %v", err)
	}

	// 4. Same check for Codex TOML when user adds a [mcp_servers.aid.env] subtable.
	codexTOML := filepath.Join(home, ".codex", "config.toml")
	pCodex, err := makePlan(ps, tools, map[string]bool{"codex": true}, stateFile, bin1)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, pCodex)
	put(t, codexTOML, append(read(t, codexTOML), []byte("\n[mcp_servers.aid.env]\nAID_CONFIG = \"/custom.yml\"\n")...))
	if _, err := makePlan(ps, tools, map[string]bool{"codex": false}, stateFile, bin1); err == nil || !strings.Contains(err.Error(), "was edited") {
		t.Fatalf("expected edited Codex TOML subtable error, got %v", err)
	}

	// 5. Corrupt JSON in settings.json is refused rather than overwritten.
	geminiSettings := filepath.Join(home, ".gemini", "settings.json")
	put(t, geminiSettings, []byte(`{invalid json`))
	if _, err := makePlan(ps, tools, map[string]bool{"gemini": true}, stateFile, bin1); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("corrupt JSON accepted: %v", err)
	}
}

func TestMCPRollbackRestoresOriginalSharedConfig(t *testing.T) {
	home := isolateHome(t)
	ps, err := providers()
	if err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(home, "state", "AID", "installer.json")
	bin1 := filepath.Join(home, "bin1")
	tools := []catalog.Tool{{ID: "diff", Command: "AID.diff", Skill: []byte("# Diff\n")}}

	cursorMCP := filepath.Join(home, ".cursor", "mcp.json")
	original := []byte("{\n  \"mcpServers\": {\n    \"other\": {\n      \"command\": \"/bin/other\"\n    }\n  }\n}\n")
	put(t, cursorMCP, original)

	p, err := makePlan(ps, tools, map[string]bool{"cursor": true}, stateFile, bin1)
	if err != nil {
		t.Fatal(err)
	}
	// Duplicate the first operation so apply fails mid-flight after modifying the first file.
	p.Operations = append(p.Operations, p.Operations[0])
	if err := p.apply(); err == nil {
		t.Fatal("expected mid-flight apply failure")
	}
	if !bytes.Equal(read(t, cursorMCP), original) {
		t.Fatalf("rollback failed to restore original mcp.json:\n%s", read(t, cursorMCP))
	}
}

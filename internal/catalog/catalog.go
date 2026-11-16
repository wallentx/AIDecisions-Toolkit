// Package catalog discovers manifests and skills shipped with this suite.
package catalog

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	assets "aid"
	"golang.org/x/mod/semver"
)

type Tool struct {
	ID          string     `json:"id"`
	Command     string     `json:"command"`
	Version     string     `json:"version"`
	Description string     `json:"description"`
	Package     string     `json:"package"`
	Collector   *Collector `json:"collector,omitempty"`
	Skill       []byte     `json:"-"`
}

// Collector declares a cheap, read-only run that the AID MCP server may
// execute on request. Args are fixed; the server appends only
// WorkspaceFlag with the client's workspace root, and TimeoutFlag with a
// collection budget shorter than TimeoutSeconds.
type Collector struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Args           []string `json:"args"`
	WorkspaceFlag  string   `json:"workspace_flag,omitempty"`
	TimeoutFlag    string   `json:"timeout_flag,omitempty"`
	TTLSeconds     int      `json:"ttl_seconds"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

var (
	idPattern            = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	collectorNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	collectorArgPattern  = regexp.MustCompile(`^--[a-z][a-z0-9-]*(=[A-Za-z0-9._,:/@-]*)?$`)
	collectorFlagPattern = regexp.MustCompile(`^--[a-z][a-z0-9-]*$`)
)

func validateCollector(collector *Collector) error {
	if collector == nil {
		return nil
	}
	if !collectorNamePattern.MatchString(collector.Name) || collector.Description == "" || len(collector.Args) > 32 {
		return fmt.Errorf("invalid collector %q", collector.Name)
	}
	hasJSON := false
	for _, arg := range collector.Args {
		if !collectorArgPattern.MatchString(arg) {
			return fmt.Errorf("collector %s has an invalid argument", collector.Name)
		}
		hasJSON = hasJSON || arg == "--json"
	}
	if !hasJSON {
		return fmt.Errorf("collector %s must request --json output", collector.Name)
	}
	if collector.WorkspaceFlag != "" && !collectorFlagPattern.MatchString(collector.WorkspaceFlag) {
		return fmt.Errorf("collector %s has an invalid workspace_flag", collector.Name)
	}
	if collector.TimeoutFlag != "" && (!collectorFlagPattern.MatchString(collector.TimeoutFlag) || collector.TimeoutFlag == collector.WorkspaceFlag) {
		return fmt.Errorf("collector %s has an invalid timeout_flag", collector.Name)
	}
	if collector.TTLSeconds < 0 || collector.TTLSeconds > 86400 || collector.TimeoutSeconds < 1 || collector.TimeoutSeconds > 3600 {
		return fmt.Errorf("collector %s needs ttl_seconds 0-86400 and timeout_seconds 1-3600", collector.Name)
	}
	return nil
}

// checkCollectorNames rejects two tools declaring the same collector name.
func checkCollectorNames(tools []Tool) error {
	seen := map[string]bool{}
	for _, tool := range tools {
		if tool.Collector == nil {
			continue
		}
		if seen[tool.Collector.Name] {
			return fmt.Errorf("duplicate collector name %q", tool.Collector.Name)
		}
		seen[tool.Collector.Name] = true
	}
	return nil
}

func parse(data, skill []byte) (Tool, error) {
	var tool Tool
	if err := json.Unmarshal(data, &tool); err != nil {
		return tool, err
	}
	if !idPattern.MatchString(tool.ID) || tool.Command != "AID."+tool.ID || tool.Package != "./cmd/"+tool.Command || !semver.IsValid("v"+tool.Version) || tool.Description == "" {
		return tool, fmt.Errorf("invalid tool manifest for %q", tool.ID)
	}
	if err := validateCollector(tool.Collector); err != nil {
		return tool, err
	}
	if len(skill) == 0 {
		return tool, fmt.Errorf("missing agent skill for %s", tool.Command)
	}
	tool.Skill = skill
	return tool, nil
}

// All returns metadata embedded in the installed AID executable.
func All() ([]Tool, error) {
	return fromFS(assets.Tools)
}

func fromFS(source fs.FS) ([]Tool, error) {
	paths, err := fs.Glob(source, "tools/*/tool.json")
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no tool manifests found")
	}
	var result []Tool
	seen := map[string]bool{}
	for _, path := range paths {
		data, err := fs.ReadFile(source, path)
		if err != nil {
			return nil, err
		}
		parts := strings.Split(path, "/")
		skill, err := fs.ReadFile(source, "tools/"+parts[1]+"/SKILL.md")
		if err != nil {
			return nil, err
		}
		tool, err := parse(data, skill)
		if err != nil {
			return nil, err
		}
		if tool.ID != parts[1] || seen[tool.ID] {
			return nil, fmt.Errorf("duplicate or misplaced manifest %s", path)
		}
		seen[tool.ID] = true
		result = append(result, tool)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	if err := checkCollectorNames(result); err != nil {
		return nil, err
	}
	return result, nil
}

// FromDirectory discovers the release source's tools, including tools newer
// than the currently installed launcher. Only conventional package paths work.
func FromDirectory(root string) ([]Tool, error) {
	paths, err := filepath.Glob(filepath.Join(root, "tools", "*", "tool.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no tool manifests in %s", root)
	}
	var result []Tool
	seen := map[string]bool{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		skill, err := os.ReadFile(filepath.Join(filepath.Dir(path), "SKILL.md"))
		if err != nil {
			return nil, err
		}
		tool, err := parse(data, skill)
		if err != nil {
			return nil, err
		}
		if filepath.Base(filepath.Dir(path)) != tool.ID || seen[tool.ID] {
			return nil, fmt.Errorf("duplicate or misplaced manifest %s", path)
		}
		seen[tool.ID] = true
		result = append(result, tool)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	if err := checkCollectorNames(result); err != nil {
		return nil, err
	}
	return result, nil
}

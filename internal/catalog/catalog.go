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
	ID          string `json:"id"`
	Command     string `json:"command"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Package     string `json:"package"`
	Skill       []byte `json:"-"`
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func parse(data, skill []byte) (Tool, error) {
	var tool Tool
	if err := json.Unmarshal(data, &tool); err != nil {
		return tool, err
	}
	if !idPattern.MatchString(tool.ID) || tool.Command != "AID."+tool.ID || tool.Package != "./cmd/"+tool.Command || !semver.IsValid("v"+tool.Version) || tool.Description == "" {
		return tool, fmt.Errorf("invalid tool manifest for %q", tool.ID)
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
	return result, nil
}

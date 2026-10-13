package discover

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"aid/internal/catalog"
	"go.yaml.in/yaml/v3"
)

const inventoryMetadataLimit = 128 << 10
const inventoryFileLimit = 1 << 20
const inventoryEntryLimit = 1000

// Inventory contains metadata only; paths and skill bodies are never included.
type Inventory struct {
	Items      []InventoryItem `json:"items"`
	SkillsSeen int             `json:"skills_seen"`
	Complete   bool            `json:"complete"`
	Warnings   []string        `json:"warnings,omitempty"`
}

type InventoryItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
}

// LoadInventory reads embedded AID manifests, installed skill frontmatter for
// the selected provider, and an optional explicit JSON metadata inventory.
func LoadInventory(provider, project, extraFile string) (Inventory, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Inventory{Complete: false, Warnings: []string{"Installed skill directories could not be located."}}, errors.New("cannot locate installed skill directories")
	}
	return loadInventoryAtHome(provider, project, extraFile, home)
}

func loadInventoryAtHome(provider, project, extraFile, home string) (Inventory, error) {
	inv := Inventory{Items: []InventoryItem{}, Complete: true}
	seen := map[string]bool{}
	add := func(name, description, source string) {
		name, description = strings.TrimSpace(Redact(name)), strings.TrimSpace(Redact(description))
		key := name + "\x00" + description
		if seen[key] {
			return
		}
		seen[key] = true
		digest := sha256.Sum256([]byte(source + "\x00" + key))
		inv.Items = append(inv.Items, InventoryItem{ID: hex.EncodeToString(digest[:]), Name: name, Description: description, Source: source})
	}
	warn := func(message string) {
		inv.Complete = false
		for _, prior := range inv.Warnings {
			if prior == message {
				return
			}
		}
		inv.Warnings = append(inv.Warnings, message)
	}
	manifests, err := catalog.All()
	if err != nil {
		warn("The embedded AID inventory could not be loaded.")
		return inv, errors.New("cannot load embedded AID inventory")
	}
	for _, tool := range manifests {
		add(tool.Command, tool.Description, "aid_tool")
	}
	roots, supported := inventoryRoots(provider, project, home)
	if !supported {
		warn("Installed skill lookup is incomplete for this provider.")
	}
	visited := map[string]bool{}
	entries := 0
	var visit func(string, int, string, bool)
	visit = func(path string, depth int, boundary string, projectRoot bool) {
		if depth > 16 {
			warn("Installed skill directory depth limit reached.")
			return
		}
		canonical, err := filepath.EvalSymlinks(path)
		if os.IsNotExist(err) {
			if depth > 0 {
				warn("An installed skill directory could not be inspected.")
			}
			return
		}
		if err != nil {
			warn("An installed skill directory could not be inspected.")
			return
		}
		if !inventoryPathWithin(boundary, canonical) {
			if projectRoot {
				warn("A project skill link escapes its configured skill directory.")
				return
			}
			// Global directory links intentionally register shared skill trees.
			boundary = canonical
		}
		if visited[canonical] {
			return
		}
		visited[canonical] = true
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() {
			warn("An installed skill directory could not be inspected.")
			return
		}
		skillPath := filepath.Join(canonical, "SKILL.md")
		metadataPath, err := filepath.EvalSymlinks(skillPath)
		if err == nil {
			if !inventoryPathWithin(boundary, metadataPath) {
				warn("An installed skill metadata link escapes its configured skill directory.")
				return
			}
			skillInfo, err := os.Stat(metadataPath)
			if err != nil {
				warn("An installed skill metadata file could not be inspected.")
				return
			}
			if !skillInfo.Mode().IsRegular() {
				warn("An installed skill metadata file is not regular.")
				return
			}
			inv.SkillsSeen++
			name, description, err := readSkillMetadata(metadataPath)
			if err != nil {
				warn("An installed skill has unreadable, invalid, or oversized frontmatter.")
				return
			}
			add(name, description, "installed_skill")
			return
		}
		if !os.IsNotExist(err) {
			warn("An installed skill metadata file could not be inspected.")
			return
		}
		remaining := inventoryEntryLimit - entries
		if remaining <= 0 {
			warn("Installed skill directory entry limit reached.")
			return
		}
		dir, err := os.Open(canonical)
		if err != nil {
			warn("An installed skill directory could not be read.")
			return
		}
		children, readErr := dir.ReadDir(remaining + 1)
		dir.Close()
		if readErr != nil && readErr != io.EOF {
			warn("An installed skill directory could not be read.")
			return
		}
		if len(children) > remaining {
			children = children[:remaining]
			warn("Installed skill directory entry limit reached.")
		}
		entries += len(children)
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, child := range children {
			if child.IsDir() || child.Type()&os.ModeSymlink != 0 {
				visit(filepath.Join(canonical, child.Name()), depth+1, boundary, projectRoot)
			}
		}
	}
	for _, root := range roots {
		canonical, err := filepath.EvalSymlinks(root.path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			warn("An installed skill directory could not be inspected.")
			continue
		}
		if root.project {
			projectPath, err := filepath.EvalSymlinks(project)
			if err != nil || !inventoryPathWithin(projectPath, canonical) {
				warn("A project skill root escapes the project directory.")
				continue
			}
		}
		visit(canonical, 0, canonical, root.project)
	}
	if extraFile != "" {
		items, err := readProvidedInventory(extraFile)
		if err != nil {
			warn("The provided inventory could not be loaded.")
			return inv, err
		}
		for _, item := range items {
			add(item.Name, item.Description, "provided_inventory")
		}
	}
	return inv, nil
}

type inventoryRoot struct {
	path    string
	project bool
}

func inventoryPathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Keep provider paths aligned with internal/installer's provider registry.
func inventoryRoots(provider, project, home string) ([]inventoryRoot, bool) {
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(home, ".config")
	}
	var base, env, projectDir string
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		base, env, projectDir = filepath.Join(home, ".codex"), "CODEX_HOME", ".codex"
	case "claude":
		base, env, projectDir = filepath.Join(home, ".claude"), "CLAUDE_CONFIG_DIR", ".claude"
	case "cursor":
		base, projectDir = filepath.Join(home, ".cursor"), ".cursor"
	case "gemini":
		base, projectDir = filepath.Join(home, ".gemini"), ".gemini"
	case "antigravity":
		base, projectDir = filepath.Join(home, ".gemini", "antigravity-cli"), filepath.Join(".gemini", "antigravity-cli")
	case "opencode":
		base, projectDir = filepath.Join(config, "opencode"), ".opencode"
	case "copilot":
		base, env, projectDir = filepath.Join(home, ".copilot"), "COPILOT_HOME", ".copilot"
	}
	roots := []inventoryRoot{{path: filepath.Join(home, ".agents", "skills")}}
	supported := base != ""
	if env != "" && os.Getenv(env) != "" {
		base = os.Getenv(env)
	}
	if supported {
		if filepath.IsAbs(base) {
			roots = append(roots, inventoryRoot{path: filepath.Join(base, "skills")})
		} else {
			supported = false
		}
	}
	if project != "" {
		roots = append(roots, inventoryRoot{path: filepath.Join(project, ".agents", "skills"), project: true})
		if projectDir != "" {
			roots = append(roots, inventoryRoot{path: filepath.Join(project, projectDir, "skills"), project: true})
		}
	}
	return roots, supported
}

// Retain only bounded frontmatter. Buffered read-ahead is discarded after its closing delimiter.
func readSkillMetadata(path string) (string, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", errors.New("cannot read skill metadata")
	}
	defer file.Close()
	var front strings.Builder
	reader := bufio.NewReaderSize(io.LimitReader(file, inventoryMetadataLimit+1), 4096)
	consumed, lines := 0, 0
	for consumed < inventoryMetadataLimit {
		line, err := reader.ReadString('\n')
		consumed += len(line)
		if consumed > inventoryMetadataLimit {
			return "", "", errors.New("oversized skill frontmatter")
		}
		if err != nil && err != io.EOF {
			return "", "", errors.New("cannot read skill metadata")
		}
		text := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		lines++
		if lines == 1 {
			if text != "---" {
				return "", "", errors.New("missing skill frontmatter")
			}
		} else if text == "---" || text == "..." {
			var metadata struct {
				Name        string `yaml:"name"`
				Description string `yaml:"description"`
			}
			if err := yaml.Unmarshal([]byte(front.String()), &metadata); err != nil || strings.TrimSpace(metadata.Name) == "" || strings.TrimSpace(metadata.Description) == "" {
				return "", "", errors.New("invalid skill frontmatter")
			}
			return metadata.Name, metadata.Description, nil
		} else {
			front.WriteString(text)
			front.WriteByte('\n')
		}
		if err == io.EOF {
			break
		}
	}
	return "", "", errors.New("missing or oversized skill frontmatter")
}

func readProvidedInventory(path string) ([]InventoryItem, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("cannot read provided inventory")
	}
	if info.Size() > inventoryFileLimit {
		return nil, errors.New("provided inventory exceeds size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read provided inventory")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, inventoryFileLimit+1))
	if err != nil || len(data) > inventoryFileLimit {
		return nil, errors.New("cannot read bounded provided inventory")
	}
	var items []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&items); err != nil || items == nil {
		return nil, errors.New("provided inventory must be an array of name and description objects")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("provided inventory contains trailing data")
	}
	if len(items) > inventoryEntryLimit {
		return nil, errors.New("provided inventory exceeds entry limit")
	}
	result := make([]InventoryItem, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.Name) == "" || strings.TrimSpace(item.Description) == "" {
			return nil, errors.New("provided inventory requires nonempty names and descriptions")
		}
		result = append(result, InventoryItem{Name: item.Name, Description: item.Description})
	}
	return result, nil
}

// OverlapHints ranks shared lexical tokens. It does not judge equivalence or novelty.
func OverlapHints(terms []string, inventory Inventory, limit int) []InventoryItem {
	if limit <= 0 {
		return nil
	}
	tokens := func(value string) []string {
		return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	}
	wanted := map[string]bool{}
	for _, term := range terms {
		for _, token := range tokens(term) {
			wanted[token] = true
		}
	}
	type scored struct {
		item  InventoryItem
		score int
	}
	var matches []scored
	for _, item := range inventory.Items {
		seen := map[string]bool{}
		score := 0
		for _, token := range tokens(item.Name + " " + item.Description) {
			if wanted[token] && !seen[token] {
				score++
				seen[token] = true
			}
		}
		if score > 0 {
			matches = append(matches, scored{item, score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	if len(matches) > limit {
		matches = matches[:limit]
	}
	result := make([]InventoryItem, 0, len(matches))
	for _, match := range matches {
		result = append(result, match.item)
	}
	return result
}

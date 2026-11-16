package reviewqueue

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aid/internal/bounded"
)

const (
	maxGitFileBytes  = 1 << 20
	maxChildEntries  = 4096
	maxAncestorSteps = 256
)

// inferScope applies the documented owner-inference order. It reads only local
// git metadata files and never runs git or contacts the network.
func inferScope(flagOwners, configOwners []string, workspace, host string) (owners []string, source string) {
	if len(flagOwners) > 0 {
		return uniqueOwners(flagOwners), ScopeFlag
	}
	if len(configOwners) > 0 {
		return uniqueOwners(configOwners), ScopeConfig
	}
	if workspace == "" {
		return nil, ScopeAll
	}
	if gitDir, ok := findGitDir(workspace); ok {
		if owner, ok := originOwner(gitDir, host); ok {
			return []string{owner}, ScopeWorkspaceRepo
		}
	}
	if owner, ok := childMajority(workspace, host); ok {
		return []string{owner}, ScopeWorkspaceChildren
	}
	return nil, ScopeAll
}

func uniqueOwners(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		key := strings.ToLower(value)
		if !seen[key] {
			seen[key] = true
			result = append(result, value)
		}
	}
	return result
}

// findGitDir walks from dir toward the filesystem root, like git's discovery,
// and returns the first valid git directory. A .git entry without HEAD is not
// a repository and discovery continues upward.
func findGitDir(dir string) (string, bool) {
	for step := 0; step < maxAncestorSteps; step++ {
		if gitDir, ok := repoGitDir(dir); ok {
			return gitDir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

// repoGitDir resolves dir/.git as a directory or as a "gitdir:" file, which
// worktrees and submodules use.
func repoGitDir(dir string) (string, bool) {
	dotGit := filepath.Join(dir, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return "", false
	}
	gitDir := dotGit
	if !info.IsDir() {
		data, err := bounded.File(dotGit, 4096, "gitdir file")
		if err != nil {
			return "", false
		}
		line, _, _ := strings.Cut(string(data), "\n")
		target, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
		target = strings.TrimSpace(target)
		if !ok || target == "" {
			return "", false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		gitDir = filepath.Clean(target)
	}
	if head, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil || head.IsDir() {
		return "", false
	}
	return gitDir, true
}

// configPath returns the shared config file. Linked worktrees name the main
// repository's git directory in a commondir file.
func configPath(gitDir string) string {
	if data, err := bounded.File(filepath.Join(gitDir, "commondir"), 4096, "commondir file"); err == nil {
		common := strings.TrimSpace(string(data))
		if common != "" {
			if !filepath.IsAbs(common) {
				common = filepath.Join(gitDir, common)
			}
			return filepath.Join(filepath.Clean(common), "config")
		}
	}
	return filepath.Join(gitDir, "config")
}

func originOwner(gitDir, host string) (string, bool) {
	data, err := bounded.File(configPath(gitDir), maxGitFileBytes, "git config")
	if err != nil {
		return "", false
	}
	url, ok := originURL(data)
	if !ok {
		return "", false
	}
	return ownerFromRemote(url, host)
}

// originURL returns the first remote.origin.url value from git config text.
// It supports [remote "origin"] and the legacy [remote.origin] header, quoted
// values, and trailing comments. Include directives are not followed.
func originURL(data []byte) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 4096), maxGitFileBytes)
	inOrigin := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				inOrigin = false
				continue
			}
			header := strings.TrimSpace(line[1:end])
			inOrigin = isOriginHeader(header)
			rest := strings.TrimSpace(line[end+1:])
			if rest == "" || rest[0] == '#' || rest[0] == ';' {
				continue
			}
			line = rest
		}
		if !inOrigin {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		if parsed := configValue(value); parsed != "" {
			return parsed, true
		}
	}
	return "", false
}

func isOriginHeader(header string) bool {
	name, sub, quoted := strings.Cut(header, " ")
	if quoted {
		sub = strings.TrimSpace(sub)
		return strings.EqualFold(name, "remote") && sub == `"origin"`
	}
	section, subsection, dotted := strings.Cut(header, ".")
	return dotted && strings.EqualFold(section, "remote") && strings.EqualFold(subsection, "origin")
}

func configValue(raw string) string {
	var b strings.Builder
	inQuote := false
	for index := 0; index < len(raw); index++ {
		char := raw[index]
		switch {
		case char == '\\' && index+1 < len(raw):
			index++
			b.WriteByte(raw[index])
		case char == '"':
			inQuote = !inQuote
		case !inQuote && (char == '#' || char == ';'):
			return strings.TrimSpace(b.String())
		default:
			b.WriteByte(char)
		}
	}
	return strings.TrimSpace(b.String())
}

// ownerFromRemote extracts the owner from an HTTPS, SSH, git, or scp-like
// remote URL whose host is the configured GitHub host.
func ownerFromRemote(raw, host string) (string, bool) {
	var remoteHost, path string
	if scheme, rest, ok := strings.Cut(raw, "://"); ok {
		switch strings.ToLower(scheme) {
		case "https", "http", "ssh", "git", "git+ssh", "ssh+git":
		default:
			return "", false
		}
		authority, remainder, _ := strings.Cut(rest, "/")
		if at := strings.LastIndexByte(authority, '@'); at >= 0 {
			authority = authority[at+1:]
		}
		if colon := strings.LastIndexByte(authority, ':'); colon >= 0 {
			authority = authority[:colon]
		}
		remoteHost, path = authority, remainder
	} else {
		colon := strings.IndexByte(raw, ':')
		slash := strings.IndexByte(raw, '/')
		if colon <= 0 || (slash >= 0 && slash < colon) {
			return "", false
		}
		authority := raw[:colon]
		if at := strings.LastIndexByte(authority, '@'); at >= 0 {
			authority = authority[at+1:]
		}
		remoteHost, path = authority, raw[colon+1:]
	}
	if host == "" {
		host = "github.com"
	}
	if !strings.EqualFold(remoteHost, host) {
		return "", false
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || strings.TrimSuffix(parts[1], ".git") == "" || !validOwner(parts[0]) {
		return "", false
	}
	return parts[0], true
}

// childMajority counts origin owners among git repositories one level below
// dir and returns the owner holding a strict majority of the counted repos.
func childMajority(dir, host string) (string, bool) {
	file, err := os.Open(dir)
	if err != nil {
		return "", false
	}
	entries, err := file.ReadDir(maxChildEntries)
	file.Close()
	if err != nil && len(entries) == 0 {
		return "", false
	}
	// Directory order varies by filesystem; sort so the reported spelling of
	// an owner is deterministic.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	counts := map[string]int{}
	names := map[string]string{}
	total := 0
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		child := filepath.Join(dir, name)
		if info, err := os.Stat(child); err != nil || !info.IsDir() {
			continue
		}
		gitDir, ok := repoGitDir(child)
		if !ok {
			continue
		}
		owner, ok := originOwner(gitDir, host)
		if !ok {
			continue
		}
		key := strings.ToLower(owner)
		if _, seen := names[key]; !seen {
			names[key] = owner
		}
		counts[key]++
		total++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if counts[key]*2 > total {
			return names[key], true
		}
	}
	return "", false
}

// validOwner accepts GitHub user and organization names.
func validOwner(name string) bool {
	if name == "" || len(name) > 39 || name[0] == '-' {
		return false
	}
	for _, char := range name {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

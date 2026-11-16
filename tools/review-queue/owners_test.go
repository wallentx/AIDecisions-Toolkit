package reviewqueue

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// makeRepo creates dir/.git with HEAD and a config whose origin is url. An
// empty url writes a config without an origin remote.
func makeRepo(t *testing.T, dir, url string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/main\n")
	config := "[core]\n\tbare = false\n"
	if url != "" {
		config += "[remote \"origin\"]\n\turl = " + url + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	}
	write(t, filepath.Join(gitDir, "config"), config)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerFromRemoteForms(t *testing.T) {
	for _, test := range []struct {
		url, host, owner string
	}{
		{"https://github.com/FarmGPU/fgpu_ansible.git", "github.com", "FarmGPU"},
		{"https://token@github.com/acme/widget", "github.com", "acme"},
		{"git@github.com:acme/widget.git", "github.com", "acme"},
		{"ssh://git@github.com/acme/widget.git", "github.com", "acme"},
		{"ssh://git@github.com:22/acme/widget", "github.com", "acme"},
		{"git@GitHub.com:acme/widget.git", "github.com", "acme"},
		{"https://github.example/corp/tool.git", "github.example", "corp"},
		{"git@gitlab.com:acme/widget.git", "github.com", ""},
		{"https://github.example/corp/tool.git", "github.com", ""},
		{"git@github-work:acme/widget.git", "github.com", ""},
		{"/srv/git/widget.git", "github.com", ""},
		{"file:///srv/git/widget.git", "github.com", ""},
		{"https://github.com/acme", "github.com", ""},
		{"https://github.com/bad_owner/widget", "github.com", ""},
	} {
		owner, ok := ownerFromRemote(test.url, test.host)
		if owner != test.owner || ok != (test.owner != "") {
			t.Errorf("ownerFromRemote(%q, %q) = %q, %v; want %q", test.url, test.host, owner, ok, test.owner)
		}
	}
}

func TestOriginURLParsesConfigSyntax(t *testing.T) {
	for name, test := range map[string]struct{ config, url string }{
		"quoted value and comment":  {"[remote \"upstream\"]\n url = git@github.com:up/x\n[remote \"origin\"] # c\n  URL = \"git@github.com:acme/x.git\" ; note\n", "git@github.com:acme/x.git"},
		"legacy header":             {"[remote.origin]\nurl=https://github.com/acme/x\n", "https://github.com/acme/x"},
		"first value wins":          {"[remote \"origin\"]\nurl = https://github.com/one/x\nurl = https://github.com/two/x\n", "https://github.com/one/x"},
		"no origin":                 {"[remote \"upstream\"]\nurl = https://github.com/acme/x\n", ""},
		"case-sensitive subsection": {"[remote \"Origin\"]\nurl = https://github.com/acme/x\n", ""},
	} {
		got, ok := originURL([]byte(test.config))
		if got != test.url || ok != (test.url != "") {
			t.Errorf("%s: originURL = %q, %v; want %q", name, got, ok, test.url)
		}
	}
}

func TestInferScopeFlagAndConfigWin(t *testing.T) {
	workspace := t.TempDir()
	makeRepo(t, workspace, "git@github.com:repo-owner/x.git")
	owners, source, _ := inferScope([]string{"acme", "ACME", "other"}, []string{"configured"}, workspace, "github.com")
	if source != ScopeFlag || !reflect.DeepEqual(owners, []string{"acme", "other"}) {
		t.Errorf("flag rule = %v %s", owners, source)
	}
	owners, source, _ = inferScope(nil, []string{"configured"}, workspace, "github.com")
	if source != ScopeConfig || !reflect.DeepEqual(owners, []string{"configured"}) {
		t.Errorf("config rule = %v %s", owners, source)
	}
}

func TestInferScopeWorkspaceRepoFromSubdirectory(t *testing.T) {
	repo := t.TempDir()
	makeRepo(t, repo, "https://github.com/acme/widget.git")
	nested := filepath.Join(repo, "src", "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	owners, source, _ := inferScope(nil, nil, nested, "github.com")
	if source != ScopeWorkspaceRepo || !reflect.DeepEqual(owners, []string{"acme"}) {
		t.Errorf("workspace repo = %v %s", owners, source)
	}
}

func TestInferScopeWorktreeGitdirFile(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main")
	makeRepo(t, main, "git@github.com:acme/widget.git")
	linked := filepath.Join(main, ".git", "worktrees", "feature")
	write(t, filepath.Join(linked, "HEAD"), "ref: refs/heads/feature\n")
	write(t, filepath.Join(linked, "commondir"), "../..\n")
	worktree := filepath.Join(base, "wt", "feature")
	write(t, filepath.Join(worktree, ".git"), "gitdir: "+linked+"\n")
	owners, source, _ := inferScope(nil, nil, worktree, "github.com")
	if source != ScopeWorkspaceRepo || !reflect.DeepEqual(owners, []string{"acme"}) {
		t.Errorf("worktree = %v %s", owners, source)
	}

	relative := filepath.Join(base, "sub")
	write(t, filepath.Join(relative, ".git"), "gitdir: ../main/.git\n")
	if owners, source, _ := inferScope(nil, nil, relative, "github.com"); source != ScopeWorkspaceRepo || owners[0] != "acme" {
		t.Errorf("relative gitdir = %v %s", owners, source)
	}
}

func TestInferScopeWorkspaceChildrenMajority(t *testing.T) {
	workspace := t.TempDir()
	makeRepo(t, filepath.Join(workspace, "a"), "git@github.com:FarmGPU/a.git")
	makeRepo(t, filepath.Join(workspace, "b"), "https://github.com/farmgpu/b")
	makeRepo(t, filepath.Join(workspace, "c"), "https://github.com/someone/c")
	makeRepo(t, filepath.Join(workspace, "d"), "git@gitlab.com:elsewhere/d.git")
	makeRepo(t, filepath.Join(workspace, "e"), "")
	makeRepo(t, filepath.Join(workspace, ".hidden"), "https://github.com/someone/h")
	write(t, filepath.Join(workspace, "notes.txt"), "not a repo")
	// A stub .git without HEAD (as in a home directory) is not a repository.
	if err := os.MkdirAll(filepath.Join(workspace, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	owners, source, _ := inferScope(nil, nil, workspace, "github.com")
	if source != ScopeWorkspaceChildren || !reflect.DeepEqual(owners, []string{"FarmGPU"}) {
		t.Errorf("children = %v %s", owners, source)
	}
}

func TestInferScopeNoMajorityIsAll(t *testing.T) {
	for name, urls := range map[string][]string{
		"tie":         {"git@github.com:one/a.git", "git@github.com:two/b.git"},
		"plurality":   {"git@github.com:one/a.git", "git@github.com:two/b.git", "git@github.com:three/c.git", "git@github.com:one/d.git", "git@github.com:four/e.git"},
		"foreign":     {"git@gitlab.com:one/a.git"},
		"no children": nil,
	} {
		workspace := t.TempDir()
		for index, url := range urls {
			makeRepo(t, filepath.Join(workspace, string(rune('a'+index))), url)
		}
		if owners, source, _ := inferScope(nil, nil, workspace, "github.com"); source != ScopeAll || owners != nil {
			t.Errorf("%s: = %v %s", name, owners, source)
		}
	}
}

func TestInferScopeForeignWorkspaceRepoFallsThroughToChildren(t *testing.T) {
	workspace := t.TempDir()
	makeRepo(t, workspace, "git@gitlab.com:mono/repo.git")
	makeRepo(t, filepath.Join(workspace, "child"), "git@github.com:acme/child.git")
	if owners, source, _ := inferScope(nil, nil, workspace, "github.com"); source != ScopeWorkspaceChildren || owners[0] != "acme" {
		t.Errorf("= %v %s", owners, source)
	}
}

func TestInferScopeSkipsChildrenWhenListingIsTruncated(t *testing.T) {
	defer func(limit int) { maxChildEntries = limit }(maxChildEntries)
	maxChildEntries = 3
	workspace := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		makeRepo(t, filepath.Join(workspace, name), "git@github.com:acme/"+name+".git")
	}
	if owners, source, warnings := inferScope(nil, nil, workspace, "github.com"); source != ScopeWorkspaceChildren || owners[0] != "acme" || warnings != nil {
		t.Fatalf("at the bound = %v %s %v", owners, source, warnings)
	}
	makeRepo(t, filepath.Join(workspace, "d"), "git@github.com:other/d.git")
	owners, source, warnings := inferScope(nil, nil, workspace, "github.com")
	if source != ScopeAll || owners != nil || !reflect.DeepEqual(warnings, []string{WarningChildrenTruncated}) {
		t.Errorf("over the bound = %v %s %v", owners, source, warnings)
	}
}

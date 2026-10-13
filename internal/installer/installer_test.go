package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aid/internal/buildinfo"
	"aid/internal/catalog"
)

type fixture struct {
	root, state, bin string
	providers        []provider
	tools            []catalog.Tool
}

func setup(t *testing.T) fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		root: root, state: filepath.Join(root, "state", "installer.json"), bin: filepath.Join(root, "bin"),
		providers: []provider{
			{ID: "codex", Name: "Codex", Root: filepath.Join(root, "codex", "skills")},
			{ID: "claude", Name: "Claude Code", Root: filepath.Join(root, "claude", "skills")},
		},
		tools: []catalog.Tool{{ID: "diff", Command: "AID.diff", Skill: []byte("# Diff\n```sh\n{{AID_COMMAND}} --help\n```\n")}},
	}
}

func (f fixture) plan(t *testing.T, desired map[string]bool) plan {
	t.Helper()
	p, err := makePlan(f.providers, f.tools, desired, f.state, f.bin)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func apply(t *testing.T, p plan) {
	t.Helper()
	if err := p.apply(); err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestInstallUpdateAndScopedRemoval(t *testing.T) {
	f := setup(t)
	first := f.plan(t, map[string]bool{"codex": true, "claude": true})
	if _, err := os.Stat(filepath.Dir(f.state)); !os.IsNotExist(err) {
		t.Fatal("planning created the state directory")
	}
	apply(t, first)
	codex := skillPath(f.providers[0], f.tools[0])
	claude := skillPath(f.providers[1], f.tools[0])
	if !bytes.Contains(read(t, codex), []byte(f.bin)) {
		t.Fatal("installed skill does not use an absolute sibling executable path")
	}
	m, _, err := readManifest(f.state)
	if err != nil || len(m.Files) != 2 {
		t.Fatalf("manifest = %+v, error = %v", m, err)
	}
	originalClaude := read(t, claude)
	f.tools[0].Skill = append(f.tools[0].Skill, []byte("Updated instructions.\n")...)
	apply(t, f.plan(t, map[string]bool{"codex": true}))
	if bytes.Equal(read(t, codex), originalClaude) {
		t.Fatal("selected client's skill was not updated")
	}
	if !bytes.Equal(read(t, claude), originalClaude) {
		t.Fatal("unselected client was changed")
	}
	sibling := filepath.Join(filepath.Dir(codex), "my-notes.txt")
	put(t, sibling, []byte("keep me"))
	apply(t, f.plan(t, map[string]bool{"codex": false}))
	if _, err := os.Stat(codex); !os.IsNotExist(err) {
		t.Fatal("managed skill was not removed")
	}
	if string(read(t, sibling)) != "keep me" || !bytes.Equal(read(t, claude), originalClaude) {
		t.Fatal("removal changed unrelated files")
	}
}

func TestRefuseManagedEditsAndUnmanagedConflicts(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmanaged", true: "managed"}[managed], func(t *testing.T) {
			f := setup(t)
			if managed {
				apply(t, f.plan(t, map[string]bool{"claude": true}))
			}
			path := skillPath(f.providers[1], f.tools[0])
			put(t, path, []byte("my edits"))
			_, err := makePlan(f.providers, f.tools, map[string]bool{"codex": true, "claude": true}, f.state, f.bin)
			if err == nil {
				t.Fatal("conflict accepted")
			}
			if _, err := os.Stat(skillPath(f.providers[0], f.tools[0])); !os.IsNotExist(err) {
				t.Fatal("preflight conflict caused a partial installation")
			}
			if string(read(t, path)) != "my edits" {
				t.Fatal("overwrote conflicting content")
			}
			if managed {
				_, err := makePlan(f.providers, f.tools, map[string]bool{"claude": false}, f.state, f.bin)
				if err == nil {
					t.Fatal("removal accepted an edited managed skill")
				}
			}
		})
	}
}

func TestAdoptIdenticalUnmanagedSkill(t *testing.T) {
	f := setup(t)
	path := skillPath(f.providers[0], f.tools[0])
	put(t, path, skillContent(f.tools[0], f.bin))
	apply(t, f.plan(t, map[string]bool{"codex": true}))
	m, _, err := readManifest(f.state)
	if err != nil || len(m.Files) != 1 {
		t.Fatalf("identical skill not adopted: %+v, %v", m, err)
	}
}

func TestAdoptionRefusesEditsAfterPreview(t *testing.T) {
	f := setup(t)
	path := skillPath(f.providers[0], f.tools[0])
	put(t, path, skillContent(f.tools[0], f.bin))
	p := f.plan(t, map[string]bool{"codex": true})
	put(t, path, []byte("edits after preview"))
	if err := p.apply(); err == nil {
		t.Fatal("adopted a skill edited after preview")
	}
	if _, err := os.Stat(f.state); !os.IsNotExist(err) {
		t.Fatal("state written despite adoption conflict")
	}
}

func TestRefuseSymlinkFileAndParents(t *testing.T) {
	for _, target := range []string{"file", "parent", "state"} {
		t.Run(target, func(t *testing.T) {
			f := setup(t)
			dest := filepath.Join(f.root, "outside")
			var link string
			switch target {
			case "file":
				link = skillPath(f.providers[0], f.tools[0])
				put(t, dest, []byte("keep"))
			case "parent":
				link = f.providers[0].Root
				if err := os.Mkdir(dest, 0700); err != nil {
					t.Fatal(err)
				}
			case "state":
				link = filepath.Dir(f.state)
				if err := os.Mkdir(dest, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(dest, link); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlink privilege unavailable: %v", err)
				}
				t.Fatal(err)
			}
			_, err := makePlan(f.providers, f.tools, map[string]bool{"codex": true}, f.state, f.bin)
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("symlink not refused: %v", err)
			}
		})
	}
}

func TestPreviewRaceAndInstallerLock(t *testing.T) {
	for _, kind := range []string{"skill", "state", "lock"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			p := f.plan(t, map[string]bool{"codex": true, "claude": true})
			switch kind {
			case "skill":
				put(t, skillPath(f.providers[1], f.tools[0]), []byte("concurrent change"))
			case "state":
				put(t, f.state, []byte("concurrent state"))
			case "lock":
				if err := os.MkdirAll(f.state+".lock", 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.apply(); err == nil {
				t.Fatal("concurrent change accepted")
			}
			if _, err := os.Stat(skillPath(f.providers[0], f.tools[0])); !os.IsNotExist(err) {
				t.Fatal("race caused a partial installation")
			}
		})
	}
}

func TestRollbackRestoresPriorContent(t *testing.T) {
	f := setup(t)
	apply(t, f.plan(t, map[string]bool{"codex": true}))
	path := skillPath(f.providers[0], f.tools[0])
	before := read(t, path)
	stateBefore := read(t, f.state)
	f.tools[0].Skill = []byte("replacement")
	p := f.plan(t, map[string]bool{"codex": true})
	// Simulate a later apply precondition failing after the first write.
	p.Operations = append(p.Operations, p.Operations[0])
	if err := p.apply(); err == nil {
		t.Fatal("expected apply failure")
	}
	if !bytes.Equal(read(t, path), before) || !bytes.Equal(read(t, f.state), stateBefore) {
		t.Fatal("rollback did not restore skills and ownership state")
	}
	if _, err := os.Stat(f.state + ".lock"); !os.IsNotExist(err) {
		t.Fatal("failed installer retained its lock")
	}
}

func TestUnexpectedManifestPathIsNeverManaged(t *testing.T) {
	f := setup(t)
	outside := filepath.Join(f.root, "unrelated.txt")
	data := []byte("my unrelated file")
	put(t, outside, data)
	put(t, f.state, []byte(`{"version":1,"files":[{"provider":"codex","tool":"diff","path":`+quoteJSON(outside)+`,"sha256":"`+digest(data)+`"}]}`))
	_, err := makePlan(f.providers, f.tools, map[string]bool{"codex": false}, f.state, f.bin)
	if err == nil {
		t.Fatal("unexpected path accepted")
	}
	if !bytes.Equal(read(t, outside), data) {
		t.Fatal("unexpected path was modified")
	}
}

func quoteJSON(s string) string {
	// %q would emit Go-specific escapes for some paths; tests use JSON itself.
	b, _ := json.Marshal(s)
	return string(b)
}

func isolateHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(name, home)
	}
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "COPILOT_HOME"} {
		t.Setenv(name, "")
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	return home
}

func TestCLIDryRunAndNonTerminalDoNotWrite(t *testing.T) {
	for _, args := range [][]string{{"--provider", "codex", "--dry-run"}, {"--list"}, {}, {"--yes"}, {"--provider", "unknown"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			home := isolateHome(t)
			var out, errs bytes.Buffer
			_, applied := RunWithResult(args, nil, &out, &errs)
			if applied {
				t.Fatal("non-apply operation reported applied")
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 0 {
				t.Fatalf("operation wrote files: %v, %v", entries, err)
			}
		})
	}
}

func TestProviderOverrides(t *testing.T) {
	home := isolateHome(t)
	codex := filepath.Join(home, "custom codex")
	t.Setenv("CODEX_HOME", codex)
	if err := os.Mkdir(codex, 0700); err != nil {
		t.Fatal(err)
	}
	ps, err := providers()
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 7 || ps[0].Root != filepath.Join(codex, "skills") || !ps[0].Detected || ps[0].Installed {
		t.Fatalf("unexpected provider state: %+v", ps)
	}
	t.Setenv("CODEX_HOME", "relative")
	if _, err := providers(); err == nil {
		t.Fatal("relative override accepted")
	}
}

func TestUpdateCheckedPreservesOtherClients(t *testing.T) {
	s := screen{Providers: []provider{
		{ID: "codex", Installed: true}, {ID: "claude", Installed: true}, {ID: "gemini"},
	}, Selected: map[string]bool{"codex": true, "claude": false, "gemini": true}, Updating: true}
	desired := s.desired()
	if len(desired) != 1 || !desired["codex"] {
		t.Fatalf("update checked included uninstalled/unchecked client: %v", desired)
	}
}

func TestReviewCannotApproveHiddenChanges(t *testing.T) {
	f := setup(t)
	s := screen{Providers: f.providers, Selected: map[string]bool{"codex": true}, Review: true}
	var out bytes.Buffer
	if s.render(&out, 80, 5) || !strings.Contains(out.String(), "Resize") {
		t.Fatal("review allowed hidden changes")
	}
	out.Reset()
	if !s.render(&out, 80, 24) || s.Apply {
		t.Fatal("normal review did not default to Cancel")
	}
}

func TestNavigationKeys(t *testing.T) {
	for input, want := range map[string]key{"\x1b[A": keyUp, "\x1b[B": keyDown, "\x1b[H": keyHome, "\x1b[F": keyEnd, "\x03": keyCancel, "q": keyCancel, "\x1b": keyEscape, "u": keyUpdate, "\r": keyEnter} {
		t.Run(input, func(t *testing.T) {
			keys := make(chan byte, len(input))
			for _, b := range []byte(input) {
				keys <- b
			}
			if got := nextKey(context.Background(), keys); got != want {
				t.Fatalf("key = %v, want %v", got, want)
			}
		})
	}
}

func TestRetiredToolCleanupRequiresMatchingDigest(t *testing.T) {
	for _, edited := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "edited"}[edited], func(t *testing.T) {
			f := setup(t)
			retired := catalog.Tool{ID: "retired", Command: "AID.retired", Skill: []byte("retired tool skill")}
			f.tools = append(f.tools, retired)
			apply(t, f.plan(t, map[string]bool{"codex": true}))
			path := skillPath(f.providers[0], retired)
			if edited {
				put(t, path, []byte("my edits"))
			}
			f.tools = f.tools[:1]
			p, err := makePlan(f.providers, f.tools, map[string]bool{"codex": true}, f.state, f.bin)
			if edited {
				if err == nil {
					t.Fatal("retired edited skill accepted for deletion")
				}
				if string(read(t, path)) != "my edits" {
					t.Fatal("lost retired skill edits")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Operations) != 1 || !p.Operations[0].Remove {
				t.Fatalf("unexpected retired tool plan: %+v", p.Operations)
			}
			apply(t, p)
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("retired skill remains installed")
			}
			m, _, err := readManifest(f.state)
			if err != nil || len(m.Files) != 1 {
				t.Fatalf("retired record remains: %+v, %v", m, err)
			}
		})
	}
}

func TestMissingSiblingExecutablePreventsInstallation(t *testing.T) {
	home := isolateHome(t)
	var out, errs bytes.Buffer
	code, applied := RunWithResult([]string{"--provider", "codex"}, nil, &out, &errs)
	if code == 0 || applied || !strings.Contains(errs.String(), "required tool") {
		t.Fatalf("missing sibling accepted: code=%d, applied=%v, error=%s", code, applied, errs.String())
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("missing executable caused writes: %v, %v", entries, err)
	}
}

func TestReviewShowsEachRemoval(t *testing.T) {
	s := screen{Review: true, Plan: plan{Operations: []operation{{Action: "remove", Label: "Codex / retired"}}}}
	var out bytes.Buffer
	if !s.render(&out, 80, 24) || !strings.Contains(out.String(), "remove: Codex / retired") {
		t.Fatal("review hid retired tool removal")
	}
}

func TestRefuseStaleSiblingVersion(t *testing.T) {
	tool := catalog.Tool{ID: "diff", Command: "AID.diff", Version: "1.0.0"}
	for _, mismatch := range []string{"command", "tool", "release"} {
		t.Run(mismatch, func(t *testing.T) {
			info := buildinfo.Info{Command: tool.Command, ToolVersion: tool.Version, ReleaseVersion: buildinfo.Version}
			switch mismatch {
			case "command":
				info.Command = "AID.other"
			case "tool":
				info.ToolVersion = "0.0.0"
			case "release":
				info.ReleaseVersion = "0.0.0"
			}
			data, err := json.Marshal(info)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateToolVersion(tool, data); err == nil {
				t.Fatal("incompatible sibling accepted")
			}
		})
	}
	if runtime.GOOS == "windows" {
		return
	} // The portable validation cases above cover Windows.
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("shell unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, tool.Command)
	script := "#!" + shell + "\nprintf '%s\\n' '{\"command\":\"AID.diff\",\"tool_version\":\"0.0.0\",\"release_version\":\"" + buildinfo.Version + "\"}'\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := verifyExecutables([]catalog.Tool{tool}, dir); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("stale executable accepted: %v", err)
	}
}

package launcher

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadOnlyActionsAndAliases(t *testing.T) {
	for _, action := range []string{"-h", "--help", "-b", "--build", "-l", "--list", "-v", "--version"} {
		t.Run(action, func(t *testing.T) {
			var out, errs bytes.Buffer
			code := Run([]string{action}, nil, &out, &errs)
			if code != 0 || out.Len() == 0 {
				t.Fatalf("code=%d stderr=%s", code, &errs)
			}
		})
	}
}

func TestMachineReadableCatalog(t *testing.T) {
	var out, errs bytes.Buffer
	if code := Run([]string{"-l", "--json"}, nil, &out, &errs); code != 0 {
		t.Fatal(&errs)
	}
	var tools []struct{ Command string }
	if err := json.Unmarshal(out.Bytes(), &tools); err != nil {
		t.Fatalf("bad catalog %s", &out)
	}
	commands := make(map[string]bool)
	for _, tool := range tools {
		if commands[tool.Command] {
			t.Fatalf("duplicate catalog command %q", tool.Command)
		}
		commands[tool.Command] = true
	}
	for _, command := range []string{"AID.ci", "AID.diff", "AID.discover", "AID.pr", "AID.pr-delta", "AID.runtime"} {
		if !commands[command] {
			t.Errorf("catalog is missing %s", command)
		}
	}
}

func TestInitDryRunDoesNotCreateConfig(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base)
	t.Setenv("CODEX_HOME", filepath.Join(base, "codex"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	path := filepath.Join(base, "new.yml")
	var out, errs bytes.Buffer
	code := Run([]string{"--config", path, "--init", "--provider", "codex", "--dry-run"}, nil, &out, &errs)
	if code != 0 {
		t.Fatalf("%d %s", code, &errs)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("dry-run created config")
	}
}

func TestBuildIsIntakeNotCompilation(t *testing.T) {
	var out, errs bytes.Buffer
	if Run([]string{"--build"}, nil, &out, &errs) != 0 {
		t.Fatal(&errs)
	}
	if !strings.Contains(out.String(), "AID tool intake") || !strings.Contains(out.String(), "SKILL.md") {
		t.Fatal("missing intake")
	}
}

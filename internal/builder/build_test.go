package builder

import (
	"aid/internal/buildinfo"
	"aid/internal/catalog"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func sourceFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, id := range []string{"diff", "logs"} {
		dir := filepath.Join(root, "tools", id)
		os.MkdirAll(dir, 0755)
		metadata, _ := json.Marshal(catalog.Tool{ID: id, Command: "AID." + id, Version: "0.1.0", Description: "test", Package: "./cmd/AID." + id})
		os.WriteFile(filepath.Join(dir, "tool.json"), metadata, 0644)
		os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("test skill"), 0644)
	}
	return root
}

func TestLaterBuildFailurePreservesAllInstalledOutputs(t *testing.T) {
	out := t.TempDir()
	root := sourceFixture(t)
	for _, name := range []string{"AID", "AID.diff"} {
		os.WriteFile(filepath.Join(out, ExecutableName(name)), []byte("original"), 0755)
	}
	compile := func(ctx context.Context, root, stage, version string, tool catalog.Tool, w io.Writer) error {
		if tool.Command == "AID.logs" {
			return errors.New("compile failed")
		}
		return os.WriteFile(filepath.Join(stage, ExecutableName(tool.Command)), []byte("new"), 0755)
	}
	if err := build(context.Background(), root, out, "v0.2.0", io.Discard, compile); err == nil {
		t.Fatal("failure not propagated")
	}
	for _, name := range []string{"AID", "AID.diff"} {
		data, err := os.ReadFile(filepath.Join(out, ExecutableName(name)))
		if err != nil || string(data) != "original" {
			t.Fatalf("changed working binary %s", name)
		}
	}
}

func TestBuildPublishesEveryDiscoveredTool(t *testing.T) {
	out := t.TempDir()
	root := sourceFixture(t)
	compile := func(ctx context.Context, root, stage, version string, tool catalog.Tool, w io.Writer) error {
		return os.WriteFile(filepath.Join(stage, ExecutableName(tool.Command)), []byte(tool.Command), 0755)
	}
	if err := build(context.Background(), root, out, "", io.Discard, compile); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"AID", "AID.diff", "AID.logs"} {
		data, err := os.ReadFile(filepath.Join(out, ExecutableName(name)))
		if err != nil || string(data) != name {
			t.Fatalf("missing discovered binary %s", name)
		}
	}
}

func TestResolveVersionPrecedence(t *testing.T) {
	root := sourceFixture(t)

	// 1. Untagged non-git directory falls back to buildinfo.Version.
	got, err := ResolveVersion(root, "")
	if err != nil || got != buildinfo.Version {
		t.Fatalf("fallback version = (%q, %v), want %q", got, err, buildinfo.Version)
	}

	// 2. Git tag in sourceDir overrides fallback without needing a VERSION file.
	for _, args := range [][]string{
		{"init", "--quiet", root},
		{"-C", root, "config", "user.name", "Test"},
		{"-C", root, "config", "user.email", "test@example.com"},
		{"-C", root, "add", "."},
		{"-C", root, "commit", "--quiet", "-m", "initial"},
		{"-C", root, "tag", "v0.4.2"},
	} {
		if out, runErr := exec.Command("git", args...).CombinedOutput(); runErr != nil {
			t.Fatalf("git %v: %v: %s", args, runErr, out)
		}
	}
	got, err = ResolveVersion(root, "")
	if err != nil || got != "0.4.2" {
		t.Fatalf("git tag version = (%q, %v), want 0.4.2", got, err)
	}

	// 3. Explicit release tag (from RELEASE_TAG or AID --update) wins over git history.
	got, err = ResolveVersion(root, "v0.5.0-rc.1")
	if err != nil || got != "0.5.0-rc.1" {
		t.Fatalf("explicit release version = (%q, %v), want 0.5.0-rc.1", got, err)
	}

	// 4. Invalid explicit release tag is rejected.
	for _, bad := range []string{"vv0.1.0", "not-semver", "1.2"} {
		if _, err := ResolveVersion(root, bad); err == nil {
			t.Fatalf("accepted invalid explicit version %q", bad)
		}
	}
}

func TestPublishFailureRollsBackEarlierTargets(t *testing.T) {
	out, stage := t.TempDir(), t.TempDir()
	for _, name := range []string{"AID.diff", "AID"} {
		os.WriteFile(filepath.Join(out, ExecutableName(name)), []byte("old"), 0755)
	}
	os.WriteFile(filepath.Join(stage, ExecutableName("AID.diff")), []byte("new"), 0755)
	if err := publish(stage, out, []catalog.Tool{{Command: "AID.diff"}, {Command: "AID"}}); err == nil {
		t.Fatal("missing staged AID accepted")
	}
	for _, name := range []string{"AID.diff", "AID"} {
		data, _ := os.ReadFile(filepath.Join(out, ExecutableName(name)))
		if string(data) != "old" {
			t.Fatal("rollback lost " + name)
		}
	}
}

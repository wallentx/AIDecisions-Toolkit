package builder

import (
	"aid/internal/catalog"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func sourceFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "VERSION"), []byte("0.2.0"), 0644)
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
	if err := build(context.Background(), root, out, io.Discard, compile); err == nil {
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
	if err := build(context.Background(), root, out, io.Discard, compile); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"AID", "AID.diff", "AID.logs"} {
		data, err := os.ReadFile(filepath.Join(out, ExecutableName(name)))
		if err != nil || string(data) != name {
			t.Fatalf("missing discovered binary %s", name)
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

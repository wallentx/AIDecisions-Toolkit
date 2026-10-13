// Package builder produces and verifies native binaries from one suite snapshot.
package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"aid/internal/buildinfo"
	"aid/internal/catalog"
	"golang.org/x/mod/semver"
)

func ExecutableName(command string) string {
	if runtime.GOOS == "windows" {
		return command + ".exe"
	}
	return command
}

func Build(ctx context.Context, sourceDir, outDir string, progress io.Writer) error {
	return build(ctx, sourceDir, outDir, progress, buildOne)
}

type targetBuilder func(context.Context, string, string, string, catalog.Tool, io.Writer) error

func build(ctx context.Context, sourceDir, outDir string, progress io.Writer, compile targetBuilder) error {
	root, err := filepath.Abs(sourceDir)
	if err != nil {
		return err
	}
	out, err := filepath.Abs(outDir)
	if err != nil {
		return err
	}
	tools, err := catalog.FromDirectory(root)
	if err != nil {
		return err
	}
	versionData, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		return err
	}
	version := strings.TrimSpace(string(versionData))
	if !semver.IsValid("v" + version) {
		return fmt.Errorf("invalid suite VERSION %q", version)
	}
	if err := os.MkdirAll(out, 0755); err != nil {
		return err
	}
	lock := filepath.Join(out, ".aid-build.lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return fmt.Errorf("another build is active or lock exists: %w", err)
	}
	defer os.Remove(lock)
	stage, err := os.MkdirTemp(out, ".aid-build-")
	if err != nil {
		return err
	}
	keepStage := false
	defer func() {
		if !keepStage {
			_ = os.RemoveAll(stage)
		}
	}()
	targets := append(tools, catalog.Tool{Command: "AID", Version: version, Package: "./cmd/AID"})
	for _, tool := range targets {
		fmt.Fprintf(progress, "Building %s for %s/%s\n", tool.Command, runtime.GOOS, runtime.GOARCH)
		if err := compile(ctx, root, stage, version, tool, progress); err != nil {
			return err
		}
	}
	if err := publish(stage, out, targets); err != nil {
		keepStage = true
		return fmt.Errorf("%w; recovery files retained at %s", err, stage)
	}
	return nil
}

func buildOne(ctx context.Context, root, stage, version string, tool catalog.Tool, progress io.Writer) error {
	path := filepath.Join(stage, ExecutableName(tool.Command))
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags=-s -w -X aid/internal/buildinfo.Version="+version, "-o", path, tool.Package)
	cmd.Dir = root
	cmd.Stdout = progress
	cmd.Stderr = progress
	cmd.Env = nativeEnv()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build %s: %w", tool.Command, err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := exec.CommandContext(checkCtx, path, "--version", "--json").Output()
	if err != nil {
		return fmt.Errorf("verify %s starts: %w", tool.Command, err)
	}
	var info buildinfo.Info
	if err := json.Unmarshal(data, &info); err != nil {
		return fmt.Errorf("verify %s metadata: %w", tool.Command, err)
	}
	if info.Command != tool.Command || info.ToolVersion != tool.Version || info.ReleaseVersion != version {
		return fmt.Errorf("%s reports versions inconsistent with source manifests", tool.Command)
	}
	return nil
}

func publish(stage, out string, targets []catalog.Tool) error {
	type changed struct {
		target, backup string
		installed      bool
	}
	var changes []*changed
	for _, tool := range targets {
		target := filepath.Join(out, ExecutableName(tool.Command))
		if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular file %s", target)
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	rollback := func(cause error) error {
		for i := len(changes) - 1; i >= 0; i-- {
			change := changes[i]
			if change.installed {
				if err := os.Remove(change.target); err != nil {
					return fmt.Errorf("%v; rollback failed: %w", cause, err)
				}
			}
			if change.backup != "" {
				if err := os.Rename(change.backup, change.target); err != nil {
					return fmt.Errorf("%v; rollback failed: %w", cause, err)
				}
			}
		}
		return cause
	}
	for _, tool := range targets {
		name := ExecutableName(tool.Command)
		change := &changed{target: filepath.Join(out, name)}
		changes = append(changes, change)
		if _, err := os.Lstat(change.target); err == nil {
			backup := filepath.Join(stage, name+".previous")
			if err := os.Rename(change.target, backup); err != nil {
				return rollback(err)
			}
			change.backup = backup
		} else if !os.IsNotExist(err) {
			return rollback(err)
		}
		if err := os.Rename(filepath.Join(stage, name), change.target); err != nil {
			return rollback(err)
		}
		change.installed = true
	}
	return nil
}

func nativeEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GOOS", "GOARCH", "GOBIN":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
}

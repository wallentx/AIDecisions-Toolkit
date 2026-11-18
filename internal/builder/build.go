// Package builder produces and verifies native binaries from one suite snapshot.
package builder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// Build compiles and verifies AID and all discovered tool binaries, resolving
// the suite release version from RELEASE_TAG, Git tags, VERSION, or buildinfo.Version.
func Build(ctx context.Context, sourceDir, outDir string, progress io.Writer) error {
	return BuildRelease(ctx, sourceDir, outDir, os.Getenv("RELEASE_TAG"), progress)
}

// BuildRelease compiles and verifies AID and all discovered tool binaries using
// explicitRelease (for example "v0.2.0" or "0.2.0") when non-empty.
func BuildRelease(ctx context.Context, sourceDir, outDir, explicitRelease string, progress io.Writer) error {
	return build(ctx, sourceDir, outDir, explicitRelease, progress, buildOne)
}

type targetBuilder func(context.Context, string, string, string, catalog.Tool, io.Writer) error

// ResolveVersion determines the suite semantic version (without a leading "v")
// from explicitRelease, the repository's latest v* Git tag, an optional VERSION
// file, or buildinfo.Version.
func ResolveVersion(sourceDir, explicitRelease string) (string, error) {
	if trimmed := strings.TrimSpace(explicitRelease); trimmed != "" {
		return normalizeReleaseVersion(trimmed)
	}
	if gitVer := gitTagVersion(sourceDir); gitVer != "" {
		return gitVer, nil
	}
	versionPath := filepath.Join(sourceDir, "VERSION")
	if versionData, err := os.ReadFile(versionPath); err == nil {
		return normalizeReleaseVersion(strings.TrimSpace(string(versionData)))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return normalizeReleaseVersion(buildinfo.Version)
}

var fullSemverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

func normalizeReleaseVersion(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.HasPrefix(trimmed, "vv") {
		return "", fmt.Errorf("invalid suite release version %q", raw)
	}
	withoutV := strings.TrimPrefix(trimmed, "v")
	canonical := "v" + withoutV
	if !fullSemverPattern.MatchString(withoutV) || !semver.IsValid(canonical) {
		return "", fmt.Errorf("invalid suite release version %q", raw)
	}
	return withoutV, nil
}

func gitTagVersion(sourceDir string) string {
	absRoot, err := filepath.Abs(sourceDir)
	if err != nil {
		return ""
	}
	evalRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		evalRoot = absRoot
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	topCmd := exec.CommandContext(ctx, "git", "-C", evalRoot, "rev-parse", "--show-toplevel")
	topCmd.Env = cleanGitEnv(evalRoot)
	topOut, err := topCmd.Output()
	if err != nil {
		return ""
	}
	topDir := strings.TrimSpace(string(topOut))
	if evalTop, evalErr := filepath.EvalSymlinks(topDir); evalErr == nil {
		topDir = evalTop
	}
	if filepath.Clean(topDir) != filepath.Clean(evalRoot) {
		return ""
	}

	descCmd := exec.CommandContext(ctx, "git", "-C", evalRoot, "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*")
	descCmd.Env = cleanGitEnv(evalRoot)
	descOut, err := descCmd.Output()
	if err != nil {
		return ""
	}
	ver, err := normalizeReleaseVersion(strings.TrimSpace(string(descOut)))
	if err != nil {
		return ""
	}
	return ver
}

func cleanGitEnv(root string) []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			env = append(env, entry)
		}
	}
	return append(
		env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(root),
	)
}

func build(ctx context.Context, sourceDir, outDir, explicitRelease string, progress io.Writer, compile targetBuilder) error {
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
	version, err := ResolveVersion(root, explicitRelease)
	if err != nil {
		return err
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

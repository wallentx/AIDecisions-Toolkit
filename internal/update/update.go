// Package update installs locally built binaries from a published GitHub release.
package update

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"aid/internal/builder"
	"aid/internal/buildinfo"
	"aid/internal/catalog"
	"aid/internal/config"
)

type options struct {
	repository string
	installDir string
	current    string
	checkOnly  bool
	dryRun     bool
}

type dependencies struct {
	latest    func(context.Context, string) (release, error)
	checkout  func(context.Context, string, string, string, io.Writer) error
	build     func(context.Context, string, string, string, io.Writer) error
	tools     func(string) ([]catalog.Tool, error)
	installed func() ([]catalog.Tool, error)
	replace   func(string, []artifact) error
	schedule  func(string, string, []artifact, io.Writer) error
	goos      string
}

func defaults() dependencies {
	return dependencies{
		latest: defaultGitHub().latest,
		checkout: func(ctx context.Context, repository, tag, source string, output io.Writer) error {
			return checkoutRelease(ctx, repository, tag, source, output, runCommand)
		},
		build:     builder.BuildRelease,
		tools:     catalog.FromDirectory,
		installed: catalog.All,
		replace: func(directory string, artifacts []artifact) error {
			return replaceArtifacts(directory, artifacts, os.Rename)
		},
		schedule: scheduleReplacement,
		goos:     runtime.GOOS,
	}
}

// Run handles arguments following AID --update. Checking never downloads source
// or changes the installation; --dry-run additionally prints the build plan.
func Run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("AID --update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "AID config file")
	repository := flags.String("repo", "", "GitHub owner/repo (overrides updates.repository)")
	installDir := flags.String("install-dir", "", "binary destination (default: current AID directory)")
	checkOnly := flags.Bool("check", false, "only check for a newer published release")
	dryRun := flags.Bool("dry-run", false, "check and print the update plan without writing files")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: AID --update [--check | --dry-run] [--repo owner/repo] [--config path]")
		fmt.Fprintln(stderr, "Updates fetch release source from GitHub and build it locally with Go and Git.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "error: --update does not accept positional arguments")
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if *repository == "" {
		*repository = cfg.Updates.Repository
	}
	if *repository == "" {
		fmt.Fprintf(stderr, "error: set updates.repository to owner/repo in %s or pass --repo owner/repo\n", cfg.Path)
		return 1
	}
	explicitInstallDir := *installDir != ""
	if *installDir == "" {
		*installDir = cfg.Updates.InstallDir
	}
	if *installDir == "" {
		executable, execErr := os.Executable()
		if execErr != nil {
			fmt.Fprintln(stderr, "error: locate current AID executable:", execErr)
			return 1
		}
		// Follow an installed launcher symlink to replace its real binary.
		executable, err = filepath.EvalSymlinks(executable)
		if err == nil {
			*installDir = filepath.Dir(executable)
		}
	} else {
		*installDir, err = config.ExpandPath(*installDir)
		if err == nil && !filepath.IsAbs(*installDir) && !explicitInstallDir {
			*installDir = filepath.Join(filepath.Dir(cfg.Path), *installDir)
		}
	}
	if err == nil {
		*installDir, err = filepath.Abs(*installDir)
	}
	if err != nil {
		fmt.Fprintln(stderr, "error: resolve install directory:", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	err = perform(ctx, options{
		repository: *repository, installDir: *installDir, current: buildinfo.Version,
		checkOnly: *checkOnly, dryRun: *dryRun,
	}, defaults(), stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func perform(ctx context.Context, opts options, deps dependencies, stdout, stderr io.Writer) error {
	if !repositoryPattern.MatchString(opts.repository) {
		return fmt.Errorf("invalid repository %q; use owner/repo", opts.repository)
	}
	latest, err := deps.latest(ctx, opts.repository)
	if err != nil {
		return err
	}
	newer, err := isNewer(opts.current, latest.TagName)
	if err != nil {
		return err
	}
	if !newer {
		fmt.Fprintf(stdout, "AID %s is current (latest release: %s).\n", opts.current, latest.TagName)
		return nil
	}
	fmt.Fprintf(stdout, "Update available: AID %s -> %s\n", opts.current, latest.TagName)
	if opts.dryRun {
		fmt.Fprintf(stdout, "Would fetch https://github.com/%s.git at tag %s into a temporary directory.\n", opts.repository, latest.TagName)
		fmt.Fprintf(stdout, "Would build and verify AID and all release tools locally, then replace binaries in %s.\n", opts.installDir)
		return nil
	}
	if opts.checkOnly {
		return nil
	}
	workspace, err := os.MkdirTemp("", "aid-update-")
	if err != nil {
		return fmt.Errorf("create update workspace: %w", err)
	}
	keepWorkspace := false
	defer func() {
		if !keepWorkspace {
			_ = os.RemoveAll(workspace)
		}
	}()
	source := filepath.Join(workspace, "source")
	stage := filepath.Join(workspace, "bin")
	fmt.Fprintf(stderr, "Fetching %s release %s\n", opts.repository, latest.TagName)
	fetchCtx, cancelFetch := context.WithTimeout(ctx, 5*time.Minute)
	err = deps.checkout(fetchCtx, opts.repository, latest.TagName, source, stderr)
	cancelFetch()
	if err != nil {
		return err
	}
	tools, err := deps.tools(source)
	if err != nil {
		return err
	}
	if err := deps.build(ctx, source, stage, latest.TagName, stderr); err != nil {
		return err
	}
	artifacts := make([]artifact, 0, len(tools)+1)
	commands := make(map[string]bool)
	for _, tool := range append(tools, catalog.Tool{Command: "AID"}) {
		commands[tool.Command] = true
		name := tool.Command
		if deps.goos == "windows" {
			name += ".exe"
		}
		path := filepath.Join(stage, name)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("missing built binary %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("built binary %s is not a regular file", name)
		}
		artifacts = append(artifacts, artifact{Name: name, Path: path})
	}
	// Remove only tools this launcher knows it shipped. Unregistered AID.* files
	// may be local tools and are never swept by a filename glob.
	if deps.installed != nil {
		previous, err := deps.installed()
		if err != nil {
			return err
		}
		for _, tool := range previous {
			if !commands[tool.Command] {
				name := tool.Command
				if deps.goos == "windows" {
					name += ".exe"
				}
				artifacts = append(artifacts, artifact{Name: name, Remove: true})
			}
		}
	}
	if deps.goos == "windows" {
		if err := deps.schedule(workspace, opts.installDir, artifacts, stdout); err != nil {
			return err
		}
		keepWorkspace = true
		return nil
	}
	if err := deps.replace(opts.installDir, artifacts); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Installed AID %s and %d tool(s) in %s.\n", strings.TrimPrefix(latest.TagName, "v"), len(tools), opts.installDir)
	return nil
}

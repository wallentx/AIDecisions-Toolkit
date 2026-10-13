package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"aid/internal/builder"
	"aid/internal/buildinfo"
	"aid/internal/catalog"
	"aid/internal/config"
	"aid/internal/installer"
	"aid/internal/intake"
	"aid/internal/update"
)

const help = `AID - small tools for recurring agent tasks

Usage: AID ACTION [options]

  -b, --build    Print the agent-driven new-tool intake template
  -v, --version Show suite and individual executable versions
  -l, --list    List tools and their descriptions
  -i, --init    Install/manage agent skills in a provider picker
  -u, --update  Fetch the latest GitHub release and build it locally
  -h, --help    Show this help

Use --json with --list/--version for structured output.
Use AID --init --help or AID --update --help for action options.
Config: ~/.config/AID/config.yml (AID_CONFIG or --config PATH overrides).
To compile this checkout: go run ./cmd/aid-build
`

func Run(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, help)
		return 0
	}
	if args[0] == "__replace" {
		return update.RunReplace(args[1:], stdout, stderr)
	}
	// --config can appear before or after the action, including updater options.
	configPath := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" {
			if i+1 == len(args) {
				fmt.Fprintln(stderr, "error: --config needs a path")
				return 2
			}
			i++
			configPath = args[i]
			continue
		}
		if strings.HasPrefix(args[i], "--config=") {
			configPath = strings.TrimPrefix(args[i], "--config=")
			continue
		}
		rest = append(rest, args[i])
	}
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "error: choose an AID action")
		return 2
	}
	action, actionArgs := rest[0], rest[1:]
	switch action {
	case "-h", "--help":
		fmt.Fprint(stdout, help)
		return 0
	case "-b", "--build":
		if len(actionArgs) != 0 {
			fmt.Fprintln(stderr, "error: --build prints the intake template and accepts no extra arguments")
			return 2
		}
		_, err := io.WriteString(stdout, intake.Template)
		if err != nil {
			return fail(stderr, err)
		}
		return 0
	case "-l", "--list", "-v", "--version":
		return metadata(action, actionArgs, stdout, stderr)
	case "-i", "--init":
		code, applied := installer.RunWithResult(actionArgs, stdin, stdout, stderr)
		if code == 0 && applied {
			if err := config.EnsureDefault(configPath); err != nil {
				return fail(stderr, fmt.Errorf("skills applied, but configuration creation failed: %w", err))
			}
		}
		return code
	case "-u", "--update":
		if configPath != "" {
			actionArgs = append(actionArgs, "--config", configPath)
		}
		return update.Run(actionArgs, stdout, stderr)
	default:
		return fail(stderr, fmt.Errorf("unknown action %q; run AID --help", action))
	}
}

type toolStatus struct {
	Command         string `json:"command"`
	ToolVersion     string `json:"tool_version"`
	ReleaseVersion  string `json:"release_version,omitempty"`
	ExpectedVersion string `json:"expected_version"`
	Status          string `json:"status"`
}

func metadata(action string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("AID "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "machine-readable output")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		return fail(stderr, errors.New("unexpected positional arguments"))
	}
	tools, err := catalog.All()
	if err != nil {
		return fail(stderr, err)
	}
	if action == "-l" || action == "--list" {
		if *jsonOutput {
			return printJSON(stdout, stderr, tools)
		}
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		for _, tool := range tools {
			fmt.Fprintf(w, "%s\t%s\n", tool.Command, tool.Description)
		}
		if err := w.Flush(); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(stderr, err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fail(stderr, err)
	}
	var statuses []toolStatus
	for _, tool := range tools {
		status := toolStatus{Command: tool.Command, ExpectedVersion: tool.Version, Status: "missing"}
		path := filepath.Join(filepath.Dir(exe), builder.ExecutableName(tool.Command))
		if _, err := os.Stat(path); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			data, err := exec.CommandContext(ctx, path, "--version", "--json").Output()
			cancel()
			var info buildinfo.Info
			status.Status = "unreadable"
			if err == nil && json.Unmarshal(data, &info) == nil && info.Command == tool.Command && info.ToolVersion != "" {
				status.ToolVersion = info.ToolVersion
				status.ReleaseVersion = info.ReleaseVersion
				status.Status = "ok"
				if info.ToolVersion != tool.Version || info.ReleaseVersion != buildinfo.Version {
					status.Status = "mismatch"
				}
			}
		}
		statuses = append(statuses, status)
	}
	if *jsonOutput {
		return printJSON(stdout, stderr, struct {
			buildinfo.Info
			Tools []toolStatus `json:"tools"`
		}{buildinfo.Info{Command: "AID", ToolVersion: buildinfo.Version, ReleaseVersion: buildinfo.Version}, statuses})
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "AID\t%s\n", buildinfo.Version)
	for _, status := range statuses {
		version := status.ToolVersion
		if version == "" {
			version = "expected " + status.ExpectedVersion
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", status.Command, version, status.Status)
	}
	if err := w.Flush(); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func printJSON(stdout, stderr io.Writer, v any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(v); err != nil {
		return fail(stderr, err)
	}
	return 0
}
func fail(stderr io.Writer, err error) int { fmt.Fprintln(stderr, "error:", err); return 1 }

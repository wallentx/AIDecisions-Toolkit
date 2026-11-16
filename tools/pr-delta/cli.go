package prdelta

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"time"

	"aid/internal/buildinfo"
	"aid/internal/config"
	"aid/internal/github"

	"golang.org/x/term"
)

//go:embed tool.json
var ManifestJSON []byte

//go:embed SKILL.md
var Skill []byte

const (
	toolID  = "pr-delta"
	command = "AID.pr-delta"
)

type settings struct {
	GH               string `yaml:"gh"`
	Hostname         string `yaml:"hostname"`
	MaxPages         int    `yaml:"max_pages"`
	MaxRequests      int    `yaml:"max_requests"`
	MaxCommitPatches int    `yaml:"max_commit_patches"`
	MaxHunks         int    `yaml:"max_hunks"`
	MaxResponseBytes int64  `yaml:"max_response_bytes"`
	MaxOutputBytes   int    `yaml:"max_output_bytes"`
	TextLimit        int    `yaml:"text_limit"`
}

func defaults() settings {
	collection := DefaultOptions()
	return settings{GH: "gh", Hostname: "github.com", MaxPages: collection.MaxPages, MaxRequests: collection.MaxRequests,
		MaxCommitPatches: collection.MaxCommitPatches, MaxHunks: collection.MaxHunksPerFile,
		MaxResponseBytes: 8 << 20, MaxOutputBytes: 128 << 10, TextLimit: collection.TextLimit}
}

// Run is the AID.pr-delta entry point.
func Run(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	return run(args, stdout, stderr, func(client github.Client) API { return &client })
}

func run(args []string, stdout, stderr io.Writer, newAPI func(github.Client) API) int {
	initial := defaults()
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	repo := flags.String("repo", "", "GitHub repository OWNER/REPO")
	number := flags.Int("pr", 0, "pull request number (with --repo)")
	reviewer := flags.String("reviewer", "", "reviewer login; default is the authenticated gh user")
	since := flags.String("since-commit", "", "baseline commit SHA (overrides the reviewer's latest review)")
	host := flags.String("hostname", initial.Hostname, "GitHub host; PR URL must match this host")
	gh := flags.String("gh", initial.GH, "GitHub CLI executable")
	configPath := flags.String("config", "", "AID YAML configuration path")
	timeout := flags.Float64("timeout", 120, "total collection timeout in seconds; supports global/tool config")
	maxPages := flags.Int("max-pages", initial.MaxPages, "maximum pages per paginated source")
	maxRequests := flags.Int("max-requests", initial.MaxRequests, "maximum API requests including the final head read")
	maxPatches := flags.Int("max-commit-patches", initial.MaxCommitPatches, "maximum commit patches fetched to match rebased commits")
	maxHunks := flags.Int("max-hunks", initial.MaxHunks, "maximum changed hunks listed per file")
	maxResponse := flags.Int64("max-response-bytes", initial.MaxResponseBytes, "maximum bytes per GitHub response")
	maxOutput := flags.Int("max-output-bytes", initial.MaxOutputBytes, "maximum encoded JSON result bytes")
	textLimit := flags.Int("text-limit", initial.TextLimit, "maximum Unicode characters per reply excerpt; 0 omits text")
	dryRun := flags.Bool("dry-run", false, "preview collection without running gh or contacting GitHub")
	showVersion := flags.Bool("version", false, "print tool and suite versions")
	jsonOutput := flags.Bool("json", false, "emit JSON evidence")
	textOutput := flags.Bool("text", false, "emit a short readable summary")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: AID.pr-delta [options] https://github.com/OWNER/REPO/pull/NUMBER\n       AID.pr-delta --repo OWNER/REPO --pr NUMBER [--reviewer LOGIN] [--since-commit SHA] [options]\n\nReport what changed in a pull request since a reviewer's last review. Read-only; no model call.\nExit 0: complete; 3: incomplete, missing baseline, or head moved; 2: usage; 1: fatal error.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(interspersedFlags(flags, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 1 || (*jsonOutput && *textOutput) {
		return cliError(stderr, 2, "use one PR reference and only one output format")
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(ManifestJSON, &metadata); err != nil || metadata.Version == "" {
		return cliError(stderr, 1, "invalid embedded pr-delta manifest")
	}
	if *showVersion {
		if err := json.NewEncoder(stdout).Encode(buildinfo.Info{Command: command, ToolVersion: metadata.Version, ReleaseVersion: buildinfo.Version}); err != nil {
			return cliError(stderr, 1, "cannot write version result")
		}
		return 0
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return cliError(stderr, 2, "cannot load AID configuration")
	}
	common, err := cfg.Resolve(toolID)
	if err != nil {
		return cliError(stderr, 2, "invalid common settings for pr-delta")
	}
	configured := defaults()
	if err := cfg.DecodeTool(toolID, &configured); err != nil {
		return cliError(stderr, 2, "invalid tools.pr-delta settings")
	}
	visited := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	if !visited["hostname"] {
		*host = configured.Hostname
	}
	if !visited["gh"] {
		*gh = configured.GH
	}
	if !visited["timeout"] {
		*timeout = common.TimeoutSeconds
	}
	for name, pair := range map[string]struct {
		dst   *int
		value int
	}{
		"max-pages": {maxPages, configured.MaxPages}, "max-requests": {maxRequests, configured.MaxRequests},
		"max-commit-patches": {maxPatches, configured.MaxCommitPatches}, "max-hunks": {maxHunks, configured.MaxHunks},
		"max-output-bytes": {maxOutput, configured.MaxOutputBytes}, "text-limit": {textLimit, configured.TextLimit},
	} {
		if !visited[name] {
			*pair.dst = pair.value
		}
	}
	if !visited["max-response-bytes"] {
		*maxResponse = configured.MaxResponseBytes
	}
	if *gh == "" || math.IsNaN(*timeout) || math.IsInf(*timeout, 0) || *timeout <= 0 || *timeout > 3600 {
		return cliError(stderr, 2, "gh must be set and timeout must be greater than 0 and at most 3600 seconds")
	}
	if *maxPages < 1 || *maxPages > 100 || *maxRequests < 4 || *maxRequests > 2000 || *maxPatches < 0 || *maxPatches > 500 || *maxHunks < 1 || *maxHunks > 1000 {
		return cliError(stderr, 2, "invalid page, request, commit-patch, or hunk limit")
	}
	if *maxResponse < 4096 || *maxResponse > 32<<20 || *maxOutput < 4096 || *maxOutput > 16<<20 || *textLimit < 0 || *textLimit > 1<<16 {
		return cliError(stderr, 2, "invalid response, output, or text limit")
	}
	if *reviewer != "" && !ValidLogin(*reviewer) {
		return cliError(stderr, 2, "--reviewer must be a GitHub login")
	}
	if *since != "" && !validCommitPrefix(*since) {
		return cliError(stderr, 2, "--since-commit must be 7 to 40 hexadecimal characters")
	}
	var raw string
	if flags.NArg() == 1 {
		raw = flags.Arg(0)
	}
	ref, err := github.ParsePullRequest(raw, *repo, *number, *host)
	if err != nil {
		return cliError(stderr, 2, "use a valid HTTPS PR URL matching --hostname, or --repo OWNER/REPO --pr NUMBER")
	}
	opts := Options{Reviewer: *reviewer, SinceCommit: strings.ToLower(*since), MaxPages: *maxPages, MaxRequests: *maxRequests,
		MaxCommitPatches: *maxPatches, MaxHunksPerFile: *maxHunks, TextLimit: *textLimit}
	if *maxPatches == 0 {
		opts.MaxCommitPatches = -1
	}
	if *dryRun {
		reviewerPlan := *reviewer
		if reviewerPlan == "" {
			reviewerPlan = "authenticated gh user (GET user)"
		}
		baselinePlan := "reviewer's latest submitted non-pending review"
		if *since != "" {
			baselinePlan = "--since-commit " + strings.ToLower(*since)
		}
		preview := struct {
			SchemaVersion int                `json:"schema_version"`
			Mode          string             `json:"mode"`
			Reference     github.PullRequest `json:"reference"`
			Reviewer      string             `json:"reviewer"`
			Baseline      string             `json:"baseline"`
			ReadOnly      bool               `json:"read_only"`
			Timeout       float64            `json:"timeout_seconds"`
			Limits        map[string]int64   `json:"limits"`
		}{SchemaVersion, "dry-run", ref, reviewerPlan, baselinePlan, true, *timeout, map[string]int64{
			"max_pages": int64(*maxPages), "max_requests": int64(*maxRequests),
			"max_commit_patches": int64(*maxPatches), "max_hunks": int64(*maxHunks),
			"max_response_bytes": *maxResponse, "max_output_bytes": int64(*maxOutput), "text_limit": int64(*textLimit),
		}}
		if err := json.NewEncoder(stdout).Encode(preview); err != nil {
			return cliError(stderr, 1, "cannot write collection preview")
		}
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(*timeout*float64(time.Second)))
	defer cancel()
	fmt.Fprintf(stderr, "%s: collecting %s/%s#%d\n", command, ref.Owner, ref.Name, ref.Number)
	api := newAPI(github.Client{Executable: *gh, Host: ref.Host, MaxResponseBytes: *maxResponse})
	result := Collect(ctx, api, ref, opts)
	encoded, rendered, err := render(result, *maxOutput)
	if err != nil {
		return cliError(stderr, 1, "cannot encode evidence within output budget; increase --max-output-bytes")
	}
	format := common.Output
	if *jsonOutput {
		format = "json"
	} else if *textOutput {
		format = "text"
	} else if format == "auto" {
		format = "json"
		if file, ok := stdout.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
			format = "text"
		}
	}
	if format == "text" {
		err = writeText(stdout, rendered)
	} else {
		_, err = stdout.Write(encoded)
	}
	if err != nil {
		return cliError(stderr, 1, "cannot write evidence result")
	}
	if rendered.Complete {
		return 0
	}
	return 3
}

func validCommitPrefix(value string) bool {
	if len(value) < 7 || len(value) > 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}

func cliError(w io.Writer, code int, message string) int {
	fmt.Fprintln(w, command+":", message)
	return code
}

// interspersedFlags supports the usual "URL --json" spelling while preserving
// flag values (including values beginning with a dash) and the -- separator.
func interspersedFlags(flags *flag.FlagSet, args []string) []string {
	var options, positional []string
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			positional = append(positional, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			positional = append(positional, argument)
			continue
		}
		options = append(options, argument)
		name, _, hasValue := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		option := flags.Lookup(name)
		if hasValue || name == "h" || name == "help" || option == nil {
			continue
		}
		if boolean, ok := option.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			continue
		}
		if index+1 < len(args) {
			index++
			options = append(options, args[index])
		} else {
			return options // Preserve flag.Parse's missing-value error.
		}
	}
	return append(append(options, "--"), positional...)
}

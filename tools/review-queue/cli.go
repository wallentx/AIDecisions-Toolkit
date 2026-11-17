package reviewqueue

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
	"path/filepath"
	"strings"
	"time"

	"aid/internal/buildinfo"
	"aid/internal/config"
	"aid/internal/github"
	"aid/internal/prdelta"

	"golang.org/x/term"
)

//go:embed tool.json
var ManifestJSON []byte

//go:embed SKILL.md
var Skill []byte

const (
	toolID  = "review-queue"
	command = "AID.review-queue"
)

type settings struct {
	GH               string   `yaml:"gh"`
	Hostname         string   `yaml:"hostname"`
	Owners           []string `yaml:"owners"`
	MaxPRs           int      `yaml:"max_prs"`
	DeltaConcurrency int      `yaml:"delta_concurrency"`
	MaxResponseBytes int64    `yaml:"max_response_bytes"`
	MaxOutputBytes   int      `yaml:"max_output_bytes"`
	TitleLimit       int      `yaml:"title_limit"`
}

func defaults() settings {
	collection := DefaultOptions()
	return settings{GH: "gh", Hostname: "github.com", MaxPRs: collection.MaxPRs, DeltaConcurrency: collection.DeltaConcurrency,
		MaxResponseBytes: 8 << 20, MaxOutputBytes: 64 << 10, TitleLimit: collection.TitleLimit}
}

type ownerFlags []string

func (owners *ownerFlags) String() string { return strings.Join(*owners, ",") }

func (owners *ownerFlags) Set(value string) error {
	for _, owner := range strings.Split(value, ",") {
		if owner = strings.TrimSpace(owner); owner != "" {
			*owners = append(*owners, owner)
		}
	}
	return nil
}

// Run is the AID.review-queue entry point.
func Run(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	return run(args, stdout, stderr, func(client github.Client) API { return &client })
}

func run(args []string, stdout, stderr io.Writer, newAPI func(github.Client) API) int {
	initial := defaults()
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var owners ownerFlags
	flags.Var(&owners, "owner", "repository owner (organization or user); repeatable or comma-separated")
	reviewer := flags.String("reviewer", "", "reviewer login; default is the authenticated gh user")
	workspace := flags.String("workspace", "", "directory used for owner inference; default is the current directory")
	noDelta := flags.Bool("no-delta", false, "list pull requests without computing re-review deltas")
	maxPRs := flags.Int("max-prs", initial.MaxPRs, "maximum pull requests listed")
	host := flags.String("hostname", initial.Hostname, "GitHub host")
	gh := flags.String("gh", initial.GH, "GitHub CLI executable")
	configPath := flags.String("config", "", "AID YAML configuration path")
	timeout := flags.Float64("timeout", 120, "total collection timeout in seconds; supports global/tool config")
	concurrency := flags.Int("delta-concurrency", initial.DeltaConcurrency, "re-review deltas computed in parallel")
	maxResponse := flags.Int64("max-response-bytes", initial.MaxResponseBytes, "maximum bytes per GitHub response")
	maxOutput := flags.Int("max-output-bytes", initial.MaxOutputBytes, "maximum encoded JSON result bytes")
	titleLimit := flags.Int("title-limit", initial.TitleLimit, "maximum Unicode characters per pull request title")
	dryRun := flags.Bool("dry-run", false, "infer scope and preview the search without running gh or contacting GitHub")
	showVersion := flags.Bool("version", false, "print tool and suite versions")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	textOutput := flags.Bool("text", false, "emit one line per pull request")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: AID.review-queue [--owner OWNER]... [--reviewer LOGIN] [--workspace DIR] [--no-delta] [options]\n\nList open pull requests waiting on a reviewer, tagged new or re-review, with a delta for re-reviews.\nRead-only; no model call. Owners default to config, then the workspace's git remotes, then all repositories.\nExit 0: complete; 3: incomplete, search failed, or truncated; 2: usage; 1: fatal error.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || (*jsonOutput && *textOutput) {
		return cliError(stderr, 2, "unexpected arguments or more than one output format")
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(ManifestJSON, &metadata); err != nil || metadata.Version == "" {
		return cliError(stderr, 1, "invalid embedded review-queue manifest")
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
		return cliError(stderr, 2, "invalid common settings for review-queue")
	}
	configured := defaults()
	if err := cfg.DecodeTool(toolID, &configured); err != nil {
		return cliError(stderr, 2, "invalid tools.review-queue settings")
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
		"max-prs": {maxPRs, configured.MaxPRs}, "delta-concurrency": {concurrency, configured.DeltaConcurrency},
		"max-output-bytes": {maxOutput, configured.MaxOutputBytes}, "title-limit": {titleLimit, configured.TitleLimit},
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
	if *maxPRs < 1 || *maxPRs > 1000 || *concurrency < 1 || *concurrency > 16 || *titleLimit < 1 || *titleLimit > 4096 {
		return cliError(stderr, 2, "invalid --max-prs (1-1000), --delta-concurrency (1-16), or --title-limit (1-4096)")
	}
	if *maxResponse < 4096 || *maxResponse > 32<<20 || *maxOutput < 4096 || *maxOutput > 16<<20 {
		return cliError(stderr, 2, "invalid response or output limit")
	}
	if *reviewer != "" && !prdelta.ValidLogin(*reviewer) {
		return cliError(stderr, 2, "--reviewer must be a GitHub login")
	}
	// ParsePullRequest validates the host with the shared transport's rules;
	// the repository and number are placeholders.
	if _, err := github.ParsePullRequest("", "probe/probe", 1, *host); err != nil {
		return cliError(stderr, 2, "--hostname must be a valid host name")
	}
	for _, owner := range append(append([]string{}, owners...), configured.Owners...) {
		if !validOwner(owner) {
			return cliError(stderr, 2, "owners must be GitHub user or organization names")
		}
	}
	root, err := workspaceRoot(*workspace)
	if err != nil {
		return cliError(stderr, 2, "--workspace must be an existing directory")
	}
	scopeOwners, scopeSource, scopeWarnings := inferScope(owners, configured.Owners, root, *host)
	opts := Options{Reviewer: *reviewer, Owners: scopeOwners, ScopeSource: scopeSource, Workspace: root, Host: *host,
		MaxPRs: *maxPRs, NoDelta: *noDelta, DeltaConcurrency: *concurrency, TitleLimit: *titleLimit, ScopeWarnings: scopeWarnings}
	if *dryRun {
		reviewerPlan := *reviewer
		if reviewerPlan == "" {
			reviewerPlan = "authenticated gh user (GET user)"
		}
		preview := struct {
			SchemaVersion int              `json:"schema_version"`
			Mode          string           `json:"mode"`
			Reviewer      string           `json:"reviewer"`
			Scope         Scope            `json:"scope"`
			Delta         bool             `json:"delta"`
			ReadOnly      bool             `json:"read_only"`
			Timeout       float64          `json:"timeout_seconds"`
			Limits        map[string]int64 `json:"limits"`
			Warnings      []string         `json:"warnings,omitempty"`
		}{SchemaVersion, "dry-run", reviewerPlan,
			Scope{Owners: append([]string{}, scopeOwners...), Source: scopeSource, Workspace: root, Query: BuildQuery(orLogin(*reviewer), scopeOwners)},
			!*noDelta, true, *timeout, map[string]int64{
				"max_prs": int64(*maxPRs), "delta_concurrency": int64(*concurrency), "title_limit": int64(*titleLimit),
				"max_response_bytes": *maxResponse, "max_output_bytes": int64(*maxOutput),
			}, scopeWarnings}
		if err := json.NewEncoder(stdout).Encode(preview); err != nil {
			return cliError(stderr, 1, "cannot write collection preview")
		}
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(*timeout*float64(time.Second)))
	defer cancel()
	scopeText := "all visible repositories"
	if len(scopeOwners) > 0 {
		scopeText = strings.Join(scopeOwners, ",")
	}
	fmt.Fprintf(stderr, "%s: searching review requests in %s (%s)\n", command, scopeText, scopeSource)
	api := newAPI(github.Client{Executable: *gh, Host: *host, MaxResponseBytes: *maxResponse})
	result := Collect(ctx, api, opts)
	encoded, rendered, err := render(result, *maxOutput)
	if err != nil {
		return cliError(stderr, 1, "cannot encode result within output budget; increase --max-output-bytes")
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
		return cliError(stderr, 1, "cannot write result")
	}
	if rendered.Complete {
		return 0
	}
	return 3
}

func orLogin(reviewer string) string {
	if reviewer == "" {
		return "@me"
	}
	return reviewer
}

func workspaceRoot(dir string) (string, error) {
	if dir == "" {
		return os.Getwd()
	}
	expanded, err := config.ExpandPath(dir)
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("not a directory")
	}
	return absolute, nil
}

func cliError(w io.Writer, code int, message string) int {
	fmt.Fprintln(w, command+":", message)
	return code
}

package ci

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"aid/internal/buildinfo"
	"aid/internal/clilimits"
	"aid/internal/config"
	"aid/internal/github"

	"golang.org/x/term"
)

var cliLimits = clilimits.Table{
	"timeout":             {Min: 0, Max: 3600, OpenMin: true},
	"page-size":           {Min: 1, Max: 100},
	"max-pages":           {Min: 1, Max: 20},
	"max-jobs":            {Min: 1, Max: 500},
	"max-log-jobs":        {Min: 1, Max: 20},
	"max-requests":        {Min: 2, Max: 200},
	"max-retained-bytes":  {Min: 4096, Max: 16777216},
	"max-annotations":     {Min: 1, Max: 500},
	"max-artifacts":       {Min: 1, Max: 500},
	"max-log-bytes":       {Min: 4096, Max: 16777216},
	"max-response-bytes":  {Min: 4096, Max: 33554432},
	"max-excerpts":        {Min: 1, Max: 5},
	"context-lines":       {Min: 0, Max: 3},
	"max-excerpt-lines":   {Min: 1, Max: 60},
	"max-line-characters": {Min: 1, Max: 2000},
	"max-log-lines":       {Min: 1, Max: 100000},
	"max-output-bytes":    {Min: 4096, Max: 16777216},
}

//go:embed tool.json
var ManifestJSON []byte

//go:embed SKILL.md
var Skill []byte

type settings struct {
	GH                string `yaml:"gh"`
	Hostname          string `yaml:"hostname"`
	PageSize          int    `yaml:"page_size"`
	MaxPages          int    `yaml:"max_pages"`
	MaxJobs           int    `yaml:"max_jobs"`
	MaxLogJobs        int    `yaml:"max_log_jobs"`
	MaxRequests       int    `yaml:"max_requests"`
	MaxRetainedBytes  int    `yaml:"max_retained_bytes"`
	MaxAnnotations    int    `yaml:"max_annotations"`
	MaxArtifacts      int    `yaml:"max_artifacts"`
	MaxLogBytes       int    `yaml:"max_log_bytes"`
	MaxResponseBytes  int64  `yaml:"max_response_bytes"`
	MaxExcerpts       int    `yaml:"max_excerpts"`
	ContextLines      int    `yaml:"context_lines"`
	MaxExcerptLines   int    `yaml:"max_excerpt_lines"`
	MaxLineCharacters int    `yaml:"max_line_characters"`
	MaxLogLines       int    `yaml:"max_log_lines"`
	MaxOutputBytes    int    `yaml:"max_output_bytes"`
}

func defaults() settings {
	collection := DefaultOptions()
	return settings{
		GH: "gh", Hostname: "github.com", PageSize: collection.PerPage,
		MaxPages: collection.MaxPages, MaxJobs: collection.MaxJobs, MaxLogJobs: collection.MaxLogJobs,
		MaxRequests: collection.MaxRequests, MaxRetainedBytes: collection.MaxRetainedBytes,
		MaxAnnotations: collection.MaxAnnotations, MaxArtifacts: collection.MaxArtifacts,
		MaxLogBytes: collection.MaxLogBytes, MaxExcerpts: collection.MaxExcerpts,
		MaxResponseBytes: 8 << 20,
		ContextLines:     collection.ContextLines, MaxExcerptLines: collection.MaxExcerptLines,
		MaxLineCharacters: collection.MaxLineCharacters, MaxOutputBytes: 128 << 10,
		MaxLogLines: collection.MaxLogLines,
	}
}

func Run(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	return run(args, stdout, stderr, func(metadata, logs github.Client) API {
		return githubAPI{metadata: metadata, logs: logs}
	})
}

type githubAPI struct {
	metadata github.Client
	logs     github.Client
}

func (api githubAPI) REST(ctx context.Context, endpoint string, out any) error {
	return api.metadata.REST(ctx, endpoint, out)
}

func (api githubAPI) RawREST(ctx context.Context, endpoint string) ([]byte, error) {
	return api.logs.RawREST(ctx, endpoint)
}

func run(args []string, stdout, stderr io.Writer, newAPI func(github.Client, github.Client) API) int {
	initial := defaults()
	flags := flag.NewFlagSet("AID.ci", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repo := flags.String("repo", "", "GitHub repository OWNER/REPO")
	runID := flags.Int64("run", 0, "workflow run ID (with --repo)")
	attempt := flags.Int("attempt", 0, "workflow run attempt; 0 resolves latest once")
	host := flags.String("hostname", initial.Hostname, "GitHub host; run URL must match this host")
	gh := flags.String("gh", initial.GH, "GitHub CLI executable")
	configPath := flags.String("config", "", "AID YAML configuration path")
	timeout := flags.Float64("timeout", 120, "total collection timeout in seconds; supports global/tool config")
	pageSize := flags.Int("page-size", initial.PageSize, "items requested per API page")
	maxPages := flags.Int("max-pages", initial.MaxPages, "maximum pages per source")
	maxJobs := flags.Int("max-jobs", initial.MaxJobs, "maximum jobs retained and checked")
	maxLogJobs := flags.Int("max-log-jobs", initial.MaxLogJobs, "maximum diagnostic job logs fetched")
	maxRequests := flags.Int("max-requests", initial.MaxRequests, "maximum GitHub API requests")
	maxRetained := flags.Int("max-retained-bytes", initial.MaxRetainedBytes, "maximum normalized evidence retained")
	maxAnnotations := flags.Int("max-annotations", initial.MaxAnnotations, "maximum check-run annotations")
	maxArtifacts := flags.Int("max-artifacts", initial.MaxArtifacts, "maximum run-wide artifact records")
	maxLogBytes := flags.Int("max-log-bytes", initial.MaxLogBytes, "maximum prefix fetched per job log")
	maxResponseBytes := flags.Int64("max-response-bytes", initial.MaxResponseBytes, "maximum bytes per metadata API response")
	maxExcerpts := flags.Int("max-excerpts", initial.MaxExcerpts, "maximum selected excerpts per job")
	contextLines := flags.Int("context-lines", initial.ContextLines, "context lines around a selected marker")
	maxExcerptLines := flags.Int("max-excerpt-lines", initial.MaxExcerptLines, "maximum lines per excerpt")
	maxLineCharacters := flags.Int("max-line-characters", initial.MaxLineCharacters, "maximum Unicode characters per returned log line")
	maxLogLines := flags.Int("max-log-lines", initial.MaxLogLines, "maximum lines processed per job log")
	maxOutput := flags.Int("max-output-bytes", initial.MaxOutputBytes, "maximum encoded JSON result bytes")
	dryRun := flags.Bool("dry-run", false, "validate and preview without running gh or contacting GitHub")
	showVersion := flags.Bool("version", false, "print tool and suite versions")
	jsonOutput := flags.Bool("json", false, "emit JSON evidence")
	textOutput := flags.Bool("text", false, "emit a readable status summary")
	cliLimits.Help(flags)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: AID.ci [options] https://github.com/OWNER/REPO/actions/runs/RUN[/attempts/N|/job/JOB]\n       AID.ci --repo OWNER/REPO --run RUN [--attempt N] [options]\n\nCollect bounded, read-only GitHub Actions evidence using gh authentication. No model call.\nExit 0: complete declared scope; 3: partial/capped/changed/ongoing; 2: usage; 1: fatal/output error.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(interspersedFlags(flags, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 1 || (*jsonOutput && *textOutput) {
		return cliError(stderr, 2, "use one workflow run reference and only one output format")
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(ManifestJSON, &metadata); err != nil || metadata.Version == "" {
		return cliError(stderr, 1, "invalid embedded ci manifest")
	}
	if *showVersion {
		if err := json.NewEncoder(stdout).Encode(buildinfo.Info{Command: "AID.ci", ToolVersion: metadata.Version, ReleaseVersion: buildinfo.Version}); err != nil {
			return cliError(stderr, 1, "cannot write version result")
		}
		return 0
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return cliError(stderr, 2, "cannot load AID configuration")
	}
	common, err := cfg.Resolve("ci")
	if err != nil {
		return cliError(stderr, 2, "invalid common settings for ci")
	}
	configured := defaults()
	if err := cfg.DecodeTool("ci", &configured); err != nil {
		return cliError(stderr, 2, "invalid tools.ci settings")
	}
	visited := make(map[string]bool)
	flags.Visit(func(option *flag.Flag) { visited[option.Name] = true })
	if !visited["hostname"] {
		*host = configured.Hostname
	}
	if !visited["gh"] {
		*gh = configured.GH
	}
	if !visited["timeout"] {
		*timeout = common.TimeoutSeconds
	}
	if !visited["max-response-bytes"] {
		*maxResponseBytes = configured.MaxResponseBytes
	}
	for name, pair := range map[string]struct {
		destination *int
		value       int
	}{
		"page-size": {pageSize, configured.PageSize}, "max-pages": {maxPages, configured.MaxPages},
		"max-jobs": {maxJobs, configured.MaxJobs}, "max-log-jobs": {maxLogJobs, configured.MaxLogJobs},
		"max-requests": {maxRequests, configured.MaxRequests}, "max-retained-bytes": {maxRetained, configured.MaxRetainedBytes},
		"max-annotations": {maxAnnotations, configured.MaxAnnotations}, "max-artifacts": {maxArtifacts, configured.MaxArtifacts},
		"max-log-bytes": {maxLogBytes, configured.MaxLogBytes}, "max-excerpts": {maxExcerpts, configured.MaxExcerpts},
		"context-lines": {contextLines, configured.ContextLines}, "max-excerpt-lines": {maxExcerptLines, configured.MaxExcerptLines},
		"max-line-characters": {maxLineCharacters, configured.MaxLineCharacters}, "max-output-bytes": {maxOutput, configured.MaxOutputBytes},
		"max-log-lines": {maxLogLines, configured.MaxLogLines},
	} {
		if !visited[name] {
			*pair.destination = pair.value
		}
	}
	if *gh == "" {
		return cliError(stderr, 2, "--gh must be set")
	}
	if err := cliLimits.Validate(flags); err != nil {
		return cliError(stderr, 2, err.Error())
	}

	var raw string
	if flags.NArg() == 1 {
		raw = flags.Arg(0)
	}
	ref, err := github.ParseWorkflowRun(raw, *repo, *runID, *attempt, *host)
	if err != nil {
		return cliError(stderr, 2, "use a valid HTTPS workflow run/job URL matching --hostname, or --repo OWNER/REPO --run ID")
	}
	options := Options{PerPage: *pageSize, MaxPages: *maxPages, MaxJobs: *maxJobs, MaxLogJobs: *maxLogJobs,
		MaxRequests: *maxRequests, MaxRetainedBytes: *maxRetained, MaxAnnotations: *maxAnnotations,
		MaxArtifacts: *maxArtifacts, MaxLogBytes: *maxLogBytes, MaxExcerpts: *maxExcerpts,
		ContextLines: *contextLines, MaxExcerptLines: *maxExcerptLines, MaxLineCharacters: *maxLineCharacters}
	options.MaxLogLines = *maxLogLines
	if *dryRun {
		preview := struct {
			SchemaVersion int                `json:"schema_version"`
			Mode          string             `json:"mode"`
			Reference     github.WorkflowRun `json:"reference"`
			ReadOnly      bool               `json:"read_only"`
			Timeout       float64            `json:"timeout_seconds"`
			Limits        map[string]int64   `json:"limits"`
			Note          string             `json:"note,omitempty"`
		}{SchemaVersion: 1, Mode: "dry-run", Reference: ref, ReadOnly: true, Timeout: *timeout,
			Limits: map[string]int64{"page_size": int64(*pageSize), "max_pages": int64(*maxPages), "max_jobs": int64(*maxJobs),
				"max_log_jobs": int64(*maxLogJobs), "max_requests": int64(*maxRequests), "max_retained_bytes": int64(*maxRetained),
				"max_annotations": int64(*maxAnnotations), "max_artifacts": int64(*maxArtifacts), "max_log_bytes": int64(*maxLogBytes),
				"max_response_bytes": *maxResponseBytes, "max_excerpts": int64(*maxExcerpts), "context_lines": int64(*contextLines),
				"max_excerpt_lines": int64(*maxExcerptLines), "max_line_characters": int64(*maxLineCharacters), "max_output_bytes": int64(*maxOutput)}}
		preview.Limits["max_log_lines"] = int64(*maxLogLines)
		if ref.Attempt == 0 {
			preview.Note = "The latest attempt will be resolved once, then every attempt-specific request will use that pinned attempt."
		}
		if err := json.NewEncoder(stdout).Encode(preview); err != nil {
			return cliError(stderr, 1, "cannot write collection preview")
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(*timeout*float64(time.Second)))
	defer cancel()
	metadataClient := github.Client{Executable: *gh, Host: ref.Host, MaxResponseBytes: *maxResponseBytes}
	logClient := github.Client{Executable: *gh, Host: ref.Host, MaxResponseBytes: int64(*maxLogBytes)}
	api := newAPI(metadataClient, logClient)
	snapshot, collectErr := Collect(ctx, api, ref, options)
	if errors.Is(collectErr, ErrJobNotInAttempt) {
		return cliError(stderr, 2, "job_not_in_attempt: the job URL is not in the selected attempt; pass --attempt with the job's run attempt")
	}
	if collectErr != nil {
		return cliError(stderr, 1, "cannot collect CI evidence")
	}
	encoded, complete, err := render(snapshot, *maxOutput)
	if err != nil {
		return cliError(stderr, 1, "cannot encode evidence within output budget")
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
		err = writeText(stdout, encoded)
	} else {
		_, err = stdout.Write(encoded)
	}
	if err != nil {
		return cliError(stderr, 1, "cannot write evidence result")
	}
	if complete {
		return 0
	}
	return 3
}

func cliError(writer io.Writer, code int, message string) int {
	fmt.Fprintln(writer, "AID.ci:", message)
	return code
}

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
		if hasValue || name == "h" || name == "help" {
			continue
		}
		if option == nil {
			continue
		}
		if boolean, ok := option.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			continue
		}
		if index+1 < len(args) {
			index++
			options = append(options, args[index])
		} else {
			return options
		}
	}
	return append(append(options, "--"), positional...)
}

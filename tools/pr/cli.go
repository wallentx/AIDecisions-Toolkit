package pr

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

type settings struct {
	GH               string `yaml:"gh"`
	Hostname         string `yaml:"hostname"`
	PageSize         int    `yaml:"page_size"`
	MaxPages         int    `yaml:"max_pages"`
	MaxItems         int    `yaml:"max_items"`
	MaxComments      int    `yaml:"max_comments"`
	MaxRequests      int    `yaml:"max_requests"`
	MaxRetainedBytes int    `yaml:"max_retained_bytes"`
	MaxResponseBytes int64  `yaml:"max_response_bytes"`
	MaxOutputBytes   int    `yaml:"max_output_bytes"`
	TextLimit        int    `yaml:"text_limit"`
}

func defaults() settings {
	collection := DefaultOptions()
	return settings{GH: "gh", Hostname: "github.com", PageSize: collection.PerPage, MaxPages: collection.MaxPages,
		MaxItems: collection.MaxItems, MaxComments: collection.MaxTotalComments, MaxRequests: collection.MaxRequests,
		MaxRetainedBytes: collection.MaxRetainedBytes, MaxResponseBytes: 8 << 20,
		MaxOutputBytes: 128 << 10, TextLimit: 4000}
}

func Run(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	return run(args, stdout, stderr, func(client github.Client) API { return &client })
}

func run(args []string, stdout, stderr io.Writer, newAPI func(github.Client) API) int {
	initial := defaults()
	flags := flag.NewFlagSet("AID.pr", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repo := flags.String("repo", "", "GitHub repository OWNER/REPO")
	number := flags.Int("pr", 0, "pull request number (with --repo)")
	host := flags.String("hostname", initial.Hostname, "GitHub host; PR URL must match this host")
	gh := flags.String("gh", initial.GH, "GitHub CLI executable")
	configPath := flags.String("config", "", "AID YAML configuration path")
	timeout := flags.Float64("timeout", 120, "total collection timeout in seconds; supports global/tool config")
	pageSize := flags.Int("page-size", initial.PageSize, "items requested per API page (1-100)")
	maxPages := flags.Int("max-pages", initial.MaxPages, "maximum pages per collection")
	maxItems := flags.Int("max-items", initial.MaxItems, "maximum items per collection")
	maxComments := flags.Int("max-comments", initial.MaxComments, "maximum review-thread comments in total")
	maxRequests := flags.Int("max-requests", initial.MaxRequests, "maximum API requests including final identity read")
	maxRetained := flags.Int("max-retained-bytes", initial.MaxRetainedBytes, "maximum normalized evidence retained during collection")
	maxResponse := flags.Int64("max-response-bytes", initial.MaxResponseBytes, "maximum bytes per GitHub response")
	maxOutput := flags.Int("max-output-bytes", initial.MaxOutputBytes, "maximum encoded JSON result bytes")
	textLimit := flags.Int("text-limit", initial.TextLimit, "maximum Unicode characters per body; 0 omits body text")
	dryRun := flags.Bool("dry-run", false, "preview collection without running gh or contacting GitHub")
	showVersion := flags.Bool("version", false, "print tool and suite versions")
	jsonOutput := flags.Bool("json", false, "emit JSON evidence")
	textOutput := flags.Bool("text", false, "emit a readable status summary")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: AID.pr [options] https://github.com/OWNER/REPO/pull/NUMBER\n       AID.pr --repo OWNER/REPO --pr NUMBER [options]\n\nCollect read-only evidence using gh authentication. No model call.\nExit 0: complete; 3: incomplete/capped/changed; 2: usage; 1: fatal error.")
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
		return cliError(stderr, 1, "invalid embedded pr manifest")
	}
	if *showVersion {
		if err := json.NewEncoder(stdout).Encode(buildinfo.Info{Command: "AID.pr", ToolVersion: metadata.Version, ReleaseVersion: buildinfo.Version}); err != nil {
			return cliError(stderr, 1, "cannot write version result")
		}
		return 0
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return cliError(stderr, 2, "cannot load AID configuration")
	}
	common, err := cfg.Resolve("pr")
	if err != nil {
		return cliError(stderr, 2, "invalid common settings for pr")
	}
	configured := defaults()
	if err := cfg.DecodeTool("pr", &configured); err != nil {
		return cliError(stderr, 2, "invalid tools.pr settings")
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
		"page-size": {pageSize, configured.PageSize}, "max-pages": {maxPages, configured.MaxPages},
		"max-items": {maxItems, configured.MaxItems}, "max-comments": {maxComments, configured.MaxComments},
		"max-requests": {maxRequests, configured.MaxRequests}, "max-retained-bytes": {maxRetained, configured.MaxRetainedBytes},
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
	if *pageSize < 1 || *pageSize > 100 || *maxPages < 1 || *maxPages > 1000 || *maxItems < 1 || *maxItems > 100000 || *maxComments < 1 || *maxComments > 100000 || *maxRequests < 2 || *maxRequests > 10000 {
		return cliError(stderr, 2, "invalid page, item, comment, or request limit")
	}
	if *maxRetained < 4096 || *maxRetained > 64<<20 || *maxResponse < 4096 || *maxResponse > 32<<20 || *maxOutput < 4096 || *maxOutput > 16<<20 || *textLimit < 0 || *textLimit > 1<<20 {
		return cliError(stderr, 2, "invalid retention, response, output, or text limit")
	}
	var raw string
	if flags.NArg() == 1 {
		raw = flags.Arg(0)
	}
	ref, err := github.ParsePullRequest(raw, *repo, *number, *host)
	if err != nil {
		return cliError(stderr, 2, "use a valid HTTPS PR URL matching --hostname, or --repo OWNER/REPO --pr NUMBER")
	}
	opts := Options{PerPage: *pageSize, MaxPages: *maxPages, MaxItems: *maxItems, MaxTotalComments: *maxComments, MaxRequests: *maxRequests, MaxRetainedBytes: *maxRetained}
	if *dryRun {
		preview := struct {
			SchemaVersion int                `json:"schema_version"`
			Mode          string             `json:"mode"`
			Reference     github.PullRequest `json:"reference"`
			ReadOnly      bool               `json:"read_only"`
			Timeout       float64            `json:"timeout_seconds"`
			Limits        map[string]int64   `json:"limits"`
		}{1, "dry-run", ref, true, *timeout, map[string]int64{
			"page_size": int64(*pageSize), "max_pages": int64(*maxPages), "max_items": int64(*maxItems),
			"max_comments": int64(*maxComments), "max_requests": int64(*maxRequests),
			"max_retained_bytes": int64(*maxRetained), "max_response_bytes": *maxResponse,
			"max_output_bytes": int64(*maxOutput), "text_limit": int64(*textLimit),
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
	api := newAPI(github.Client{Executable: *gh, Host: ref.Host, MaxResponseBytes: *maxResponse})
	snapshot := Collect(ctx, api, ref, opts)
	encoded, complete, err := render(snapshot, *maxOutput, *textLimit)
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

func cliError(w io.Writer, code int, message string) int {
	fmt.Fprintln(w, "AID.pr:", message)
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
		if hasValue || name == "h" || name == "help" {
			continue
		}
		if option == nil {
			continue // flag.Parse will report the unknown option.
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

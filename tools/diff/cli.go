package diff

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"aid/internal/bounded"
	"aid/internal/buildinfo"
	"aid/internal/config"
	"aid/internal/decisions"

	"golang.org/x/term"
)

// Run executes AID.diff and returns a process exit code. Streams are injectable
// so agents and tests can use the same CLI without replacing process globals.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return run(args, stdin, stdout, stderr, nil)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, transport *http.Client) int {
	flags := flag.NewFlagSet("AID.diff", flag.ContinueOnError)
	flags.SetOutput(stderr)
	diffFile := flags.String("diff", "", "read a diff file (default: standard input)")
	staged := flags.Bool("staged", false, "read git diff --cached from the current repository")
	example := flags.Bool("example", false, "use the bundled API compatibility example")
	configFile := flags.String("config", "", "suite YAML config (default: ~/.config/AID/config.yml)")
	checksFile := flags.String("checks", "", "JSON file mapping check names to questions")
	contextText := flags.String("context", "", "additional evidence about the change")
	providerList := strings.Join(decisions.ProviderIDs(), ", ")
	envList := strings.Join(decisions.ProviderEnvVars(), ", ")
	providerFlag := flags.String("provider", "", "Decisions provider: "+providerList)
	model := flags.String("model", "", "Decisions model (default: configured or provider default)")
	endpoint := flags.String("endpoint", "", "override Decisions API endpoint URL")
	apiKeyFile := flags.String("api-key-file", "", "file containing the selected provider's API key")
	dryRun := flags.Bool("dry-run", false, "print the resolved provider target and wire request without an API call or key")
	jsonOutput := flags.Bool("json", false, "print answers as JSON")
	output := flags.String("output", "", "output format: auto, json, or text (table is an alias)")
	timeout := flags.Float64("timeout", 30, "request deadline in seconds")
	showVersion := flags.Bool("version", false, "print tool and suite release versions")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: AID.diff [options]\n\nEvaluate a diff using configurable Decisions API checks.\nProviders: %s (default: %s).\nLive requests send the diff and context to the selected provider; --dry-run previews locally.\nCredentials: %s, then provider api_key_file in suite config.\nOutput defaults to JSON for pipes and agents, or a table in a terminal.\n\n", providerList, decisions.DefaultProvider, envList)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		return fail(stderr, 2, errors.New("no positional arguments are accepted"))
	}
	if *showVersion {
		toolVersion, err := version()
		if err != nil {
			return fail(stderr, 1, err)
		}
		if *jsonOutput || *output == "json" || (*output == "" && !isTerminal(stdout)) {
			return printJSON(stdout, stderr, map[string]string{
				"command": "AID.diff", "tool_version": toolVersion, "release_version": buildinfo.Version,
			})
		}
		fmt.Fprintf(stdout, "AID.diff %s (AID release %s)\n", toolVersion, buildinfo.Version)
		return 0
	}
	cfg, err := config.Load(*configFile)
	if err != nil {
		return fail(stderr, 2, err)
	}
	visited := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	var overrides config.Overrides
	if visited["provider"] {
		overrides.Provider = providerFlag
	}
	if visited["model"] {
		overrides.Model = model
	}
	if visited["endpoint"] {
		overrides.Endpoint = endpoint
	}
	if visited["api-key-file"] {
		overrides.APIKeyFile = apiKeyFile
	}
	if visited["checks"] {
		overrides.ChecksFile = checksFile
	}
	if visited["context"] {
		overrides.Context = contextText
	}
	if visited["timeout"] {
		overrides.TimeoutSeconds = timeout
	}
	if visited["output"] {
		overrides.Output = output
	}
	if visited["json"] {
		outFmt := "text"
		if *jsonOutput {
			outFmt = "json"
		}
		overrides.Output = &outFmt
	}

	opts, err := cfg.ResolveDecisions("diff", overrides)
	if err != nil {
		return fail(stderr, 2, err)
	}

	sources := 0
	for _, selected := range []bool{*diffFile != "", *staged, *example} {
		if selected {
			sources++
		}
	}
	if sources > 1 {
		return fail(stderr, 2, errors.New("choose only one of --diff, --staged, or --example"))
	}
	var data []byte
	switch {
	case *example:
		data = []byte(ExampleDiff)
	case *diffFile != "":
		data, err = bounded.File(*diffFile, maxDiffBytes, "diff")
	case *staged:
		data, err = readStagedDiff()
	default:
		if isTerminal(stdin) {
			return fail(stderr, 2, errors.New("pipe a diff, use --diff FILE, or try --example --dry-run"))
		}
		data, err = bounded.Read(stdin, maxDiffBytes, "diff")
	}
	if errors.Is(err, bounded.ErrLimit) {
		return fail(stderr, 2, err)
	}
	if err != nil {
		return fail(stderr, 1, fmt.Errorf("read diff: %w", err))
	}
	var checks decisions.Checks
	if opts.ChecksFile != "" {
		checksData, readErr := bounded.File(opts.ChecksFile, maxChecksBytes, "checks file")
		if errors.Is(readErr, bounded.ErrLimit) {
			return fail(stderr, 2, readErr)
		}
		if readErr != nil {
			return fail(stderr, 1, readErr)
		}
		if err = json.Unmarshal(checksData, &checks); err != nil || checks == nil {
			return fail(stderr, 2, errors.New("checks must be a JSON object mapping names to question strings"))
		}
	}
	request, err := BuildRequest(string(data), opts.Context, opts.Model, checks)
	if err != nil {
		return fail(stderr, 2, err)
	}
	client := decisions.Client{
		Provider: decisions.Provider(opts.Provider),
		Model:    opts.Model,
		Endpoint: opts.Endpoint,
		HTTP:     transport,
	}
	preview, err := client.Preview(request)
	if err != nil {
		return fail(stderr, 2, err)
	}
	if *dryRun {
		return printJSON(stdout, stderr, preview)
	}
	key, err := config.APIKey(opts)
	if err != nil {
		return fail(stderr, 1, err)
	}
	client.APIKey = key

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(opts.TimeoutSeconds*float64(time.Second)))
	defer cancel()

	answers, err := client.Decide(ctx, request)
	if err != nil {
		if decisions.IsUnsupported(err) {
			return fail(stderr, 2, err)
		}
		return fail(stderr, 1, err)
	}
	refused := false
	for _, answer := range answers {
		refused = refused || answer.Type == decisions.AnswerRefusal
	}
	if opts.Output == "json" || (opts.Output == "auto" && !isTerminal(stdout)) {
		if code := printJSON(stdout, stderr, answers); code != 0 {
			return code
		}
	} else if err := printTable(stdout, answers); err != nil {
		return fail(stderr, 1, err)
	}
	if refused {
		return 3
	}
	return 0
}

func readStagedDiff() ([]byte, error) {
	check := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	out, err := check.Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return nil, errors.New("--staged must be run inside a Git repository worktree (cd into a repo or use --diff FILE)")
	}
	cmd := exec.Command("git", "diff", "--cached", "--no-ext-diff", "--no-textconv")
	buf := bounded.NewBuffer(maxDiffBytes, "staged diff")
	errBuf := bounded.NewBuffer(4096, "git diff stderr")
	cmd.Stdout = buf
	cmd.Stderr = errBuf
	runErr := cmd.Run()
	if buf.Err() != nil {
		return nil, buf.Err()
	}
	if runErr != nil {
		msg := strings.TrimSpace(string(errBuf.Bytes()))
		if msg != "" {
			return nil, fmt.Errorf("%w: %s", runErr, msg)
		}
		return nil, runErr
	}
	return buf.Bytes(), nil
}

func printTable(stdout io.Writer, answers map[string]decisions.Answer) error {
	out := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(out, "CHECK\tPROBABILITY")
	names := make([]string, 0, len(answers))
	for name := range answers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		answer := answers[name]
		if answer.Type == decisions.AnswerRefusal {
			fmt.Fprintf(out, "%s\tUNANSWERED (refusal)\n", name)
		} else {
			fmt.Fprintf(out, "%s\t%.1f%%\n", name, *answer.Probability*100)
		}
	}
	return out.Flush()
}

func isTerminal(stream any) bool {
	file, ok := stream.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(file.Fd()))
}

func printJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fail(stderr, 1, err)
	}
	return 0
}

func fail(stderr io.Writer, code int, err error) int {
	fmt.Fprintln(stderr, "error:", err)
	return code
}

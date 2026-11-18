package runtime

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
	"path/filepath"
	"strings"
	"time"

	"aid/internal/buildinfo"
	"aid/internal/clilimits"
	"aid/internal/config"

	"golang.org/x/term"
)

var cliLimits = clilimits.Table{
	"timeout":        {Min: 0, Max: 3600, OpenMin: true},
	"max-file-bytes": {Min: 0, Max: float64(MaximumMaxFileBytes)},
}

//go:embed tool.json
var ManifestJSON []byte

//go:embed SKILL.md
var Skill []byte

type settings struct {
	MaxFileBytes int64 `yaml:"max_file_bytes"`
}

func defaultSettings() settings {
	return settings{MaxFileBytes: DefaultMaxFileBytes}
}

func Run(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("AID.runtime", flag.ContinueOnError)
	flags.SetOutput(stderr)
	initial := defaultSettings()
	fileOption := flags.String("file", "", "one ELF file to inspect (exclusive with positional FILE)")
	manifestPath := flags.String("host-manifest", "", "strict JSON allowlist for opt-in host/path observation")
	configPath := flags.String("config", "", "AID YAML configuration path")
	timeout := flags.Float64("timeout", 120, "total collection timeout in seconds")
	maxFileBytes := flags.Int64("max-file-bytes", initial.MaxFileBytes, "maximum target file size; 0 uses the default")
	jsonOutput := flags.Bool("json", false, "emit canonical compact JSON evidence")
	textOutput := flags.Bool("text", false, "emit a readable static-facts summary")
	dryRun := flags.Bool("dry-run", false, "validate options and manifest without reading the target or observing host paths")
	showVersion := flags.Bool("version", false, "print tool and suite versions")
	cliLimits.Help(flags)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: AID.runtime [options] FILE\n       AID.runtime --file FILE [options]\n\nCollect bounded static facts for one Linux/Android AArch64 ELF executable or shared object. The target is never executed.\nExit 0: complete supported facts; 3: valid unsupported/malformed/incomplete evidence; 2: usage/config/manifest; 1: fatal/output error.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(interspersedFlags(flags, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *jsonOutput && *textOutput {
		return cliError(stderr, 2, "use only one output format")
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(ManifestJSON, &metadata); err != nil || metadata.Version == "" {
		return cliError(stderr, 1, "invalid embedded runtime manifest")
	}
	if *showVersion {
		if err := json.NewEncoder(stdout).Encode(buildinfo.Info{Command: "AID.runtime", ToolVersion: metadata.Version, ReleaseVersion: buildinfo.Version}); err != nil {
			return cliError(stderr, 1, "cannot write version result")
		}
		return 0
	}

	if flags.NArg() > 1 || (*fileOption != "" && flags.NArg() != 0) || (*fileOption == "" && flags.NArg() != 1) {
		return cliError(stderr, 2, "select exactly one file using FILE or --file FILE")
	}
	targetPath := *fileOption
	if targetPath == "" {
		targetPath = flags.Arg(0)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return cliError(stderr, 2, "cannot load AID configuration")
	}
	common, err := cfg.Resolve("runtime")
	if err != nil {
		return cliError(stderr, 2, "invalid common settings for runtime")
	}
	configured := defaultSettings()
	if err := cfg.DecodeTool("runtime", &configured); err != nil {
		return cliError(stderr, 2, "invalid tools.runtime settings")
	}
	visited := map[string]bool{}
	flags.Visit(func(option *flag.Flag) { visited[option.Name] = true })
	if !visited["max-file-bytes"] {
		*maxFileBytes = configured.MaxFileBytes
	}
	if !visited["timeout"] {
		*timeout = common.TimeoutSeconds
	}
	options := Options{MaxFileBytes: *maxFileBytes}
	if err := cliLimits.Validate(flags); err != nil {
		return cliError(stderr, 2, err.Error())
	}
	manifest, err := LoadHostManifest(*manifestPath)
	if err != nil {
		return cliError(stderr, 2, err.Error())
	}
	if err := PreflightEnvelope(targetPath, manifest, options); err != nil {
		return cliError(stderr, 1, err.Error())
	}
	if *dryRun {
		abs, absErr := filepath.Abs(targetPath)
		if absErr != nil {
			return cliError(stderr, 2, "cannot make target path absolute")
		}
		preview := struct {
			SchemaVersion int     `json:"schema_version"`
			Mode          string  `json:"mode"`
			Target        string  `json:"target"`
			Timeout       float64 `json:"timeout_seconds"`
			Limits        struct {
				MaxFileBytes int64 `json:"max_file_bytes"`
			} `json:"limits"`
			HostManifest *HostManifest `json:"host_manifest,omitempty"`
			Note         string        `json:"note"`
		}{SchemaVersion: 1, Mode: "dry-run", Target: abs, Timeout: *timeout, HostManifest: manifest,
			Note: "Manifest was read and validated; target and allowlisted host paths were not read or observed."}
		preview.Limits.MaxFileBytes = options.MaxFileBytes
		if err := json.NewEncoder(stdout).Encode(preview); err != nil {
			return cliError(stderr, 1, "cannot write validation preview")
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(*timeout*float64(time.Second)))
	defer cancel()
	report, err := Collect(ctx, targetPath, manifest, options)
	if err != nil {
		return cliError(stderr, 1, err.Error())
	}
	encoded, complete, err := Render(report)
	if err != nil {
		return cliError(stderr, 1, err.Error())
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
		err = WriteText(stdout, encoded)
	} else {
		_, err = stdout.Write(encoded)
	}
	if err != nil {
		return cliError(stderr, 1, "cannot write runtime evidence")
	}
	if complete {
		return 0
	}
	return 3
}

func cliError(writer io.Writer, code int, message string) int {
	fmt.Fprintln(writer, "AID.runtime:", message)
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

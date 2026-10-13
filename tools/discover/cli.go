package discover

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
	"time"

	assets "aid"
	"aid/internal/bounded"
	"aid/internal/buildinfo"
	"aid/internal/config"
)

//go:embed tool.json
var ManifestJSON []byte

//go:embed SKILL.md
var Skill []byte

func version() (string, error) {
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(ManifestJSON, &metadata); err != nil || metadata.Version == "" {
		return "", errors.New("invalid embedded discover manifest")
	}
	return metadata.Version, nil
}

type settings struct {
	Provider        string  `yaml:"provider"`
	Reflect         string  `yaml:"reflect"`
	CacheDir        string  `yaml:"cache_dir"`
	CacheTTLSeconds float64 `yaml:"cache_ttl_seconds"`
	MaxInputBytes   int64   `yaml:"max_input_bytes"`
	MaxOutputBytes  int     `yaml:"max_output_bytes"`
	MaxRecords      int     `yaml:"max_records"`
	MaxRecordBytes  int     `yaml:"max_record_bytes"`
	PageSize        int     `yaml:"page_size"`
	MinSessions     int     `yaml:"min_sessions"`
}

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("AID.discover", flag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("input", "", "normalized Reflect JSON file; - reads stdin")
	reflectPath := flags.String("reflect", "", "Reflect executable or .py helper (default: reflect on PATH)")
	provider := flags.String("provider", "codex", "history provider")
	project := flags.String("project", "", "project scope (default: current directory)")
	days := flags.Int("days", 365, "requested history window in days")
	allProjects := flags.Bool("all-projects", false, "include all project histories")
	configPath := flags.String("config", "", "AID YAML config path")
	criteriaPath := flags.String("criteria", "", "explicit criteria Markdown override (default: bundled TOOL-CRITERIA.md)")
	inventoryPath := flags.String("inventory", "", "additional JSON tool/skill metadata inventory")
	cacheDir := flags.String("cache-dir", "", "private snapshot cache directory")
	cacheTTL := flags.Duration("cache-ttl", 10*time.Minute, "source-cache reuse interval; 0 means never reuse (not retention)")
	refresh := flags.Bool("refresh", false, "scan/prepare again instead of reusing a source cache")
	noCache := flags.Bool("no-cache", false, "do not read or write cached evidence")
	snapshot := flags.String("snapshot", "", "read a frozen snapshot ID without rescanning")
	evidence := flags.String("evidence", "", "show records belonging to a group or a single record ID")
	allRecords := flags.Bool("records", false, "page all records, including ungrouped evidence")
	offset := flags.Int("offset", 0, "page offset")
	pageSize := flags.Int("page-size", 12, "maximum leads/records per page")
	minSessions := flags.Int("min-sessions", 2, "minimum conservative sessions for a lead")
	textOffset := flags.Int("text-offset", 0, "character offset within a single evidence record")
	textLimit := flags.Int("text-limit", 1600, "maximum characters per evidence excerpt")
	maxOutput := flags.Int("max-output-bytes", 32768, "maximum complete JSON output size")
	timeout := flags.Float64("timeout", 30, "source operation timeout in seconds")
	dryRun := flags.Bool("dry-run", false, "preview source options without scanning or writing cache")
	showVersion := flags.Bool("version", false, "print tool and suite release versions")
	flags.Bool("json", true, "JSON output (always enabled for evidence)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: AID.discover [--all-projects] [--days 365]\n       AID.discover --snapshot ID --evidence GROUP_OR_RECORD_ID\n\nPrepare bounded evidence for the current agent; no model call or API key.\nHistory is read through Reflect or an explicit normalized JSON input.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		return cliError(stderr, 2, "unexpected positional arguments")
	}
	ver, err := version()
	if err != nil {
		return cliError(stderr, 1, err.Error())
	}
	if *showVersion {
		return emitJSON(stdout, stderr, buildinfo.Info{Command: "AID.discover", ToolVersion: ver, ReleaseVersion: buildinfo.Version})
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return cliError(stderr, 2, "cannot load AID config")
	}
	common, err := cfg.Resolve("discover")
	if err != nil {
		return cliError(stderr, 2, "invalid discover common settings")
	}
	opts := settings{Provider: "codex", CacheTTLSeconds: 600, MaxInputBytes: 64 << 20, MaxOutputBytes: 32768, MaxRecords: 100000, MaxRecordBytes: 65536, PageSize: 12, MinSessions: 2}
	if err := cfg.DecodeTool("discover", &opts); err != nil {
		return cliError(stderr, 2, "invalid tools.discover settings")
	}
	visited := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	if !visited["provider"] {
		*provider = opts.Provider
	}
	if !visited["reflect"] {
		*reflectPath = opts.Reflect
	}
	if !visited["cache-dir"] {
		*cacheDir = opts.CacheDir
	}
	if !visited["page-size"] {
		*pageSize = opts.PageSize
	}
	if !visited["min-sessions"] {
		*minSessions = opts.MinSessions
	}
	if !visited["max-output-bytes"] {
		*maxOutput = opts.MaxOutputBytes
	}
	if math.IsNaN(opts.CacheTTLSeconds) || math.IsInf(opts.CacheTTLSeconds, 0) || opts.CacheTTLSeconds < 0 || opts.CacheTTLSeconds > 86400 {
		return cliError(stderr, 2, "cache_ttl_seconds must be 0..86400")
	}
	if !visited["cache-ttl"] {
		*cacheTTL = time.Duration(opts.CacheTTLSeconds * float64(time.Second))
	}
	if !visited["timeout"] {
		*timeout = common.TimeoutSeconds
	}
	if *days < 1 || *days > 36500 || *offset < 0 || *pageSize < 1 || *pageSize > 100 || *minSessions < 1 || *textOffset < 0 || *textLimit < 1 || *textLimit > 12000 || *maxOutput < 4096 || *maxOutput > 1<<20 || *cacheTTL < 0 || math.IsNaN(*timeout) || math.IsInf(*timeout, 0) || *timeout <= 0 || *timeout > 3600 || opts.MaxInputBytes < 1024 || opts.MaxInputBytes > 64<<20 || opts.MaxRecords < 1 || opts.MaxRecords > 1000000 || opts.MaxRecordBytes < 256 || opts.MaxRecordBytes > 1<<20 {
		return cliError(stderr, 2, "invalid limit: use --help for supported arguments; output budget must be 4096..1048576 bytes")
	}
	if *evidence != "" && *allRecords {
		return cliError(stderr, 2, "choose --evidence or --records")
	}
	if *snapshot != "" && (*input != "" || visited["reflect"] || *refresh || *noCache) {
		return cliError(stderr, 2, "--snapshot cannot be combined with a new input, refresh, or no-cache")
	}
	if *project == "" {
		*project, err = os.Getwd()
	} else {
		*project, err = config.ExpandPath(*project)
	}
	if err != nil {
		return cliError(stderr, 1, "cannot resolve project directory")
	}
	*project, err = filepath.Abs(*project)
	if err != nil {
		return cliError(stderr, 1, "cannot resolve project directory")
	}
	for _, path := range []*string{cacheDir, criteriaPath, inventoryPath, reflectPath, input} {
		if *path != "" {
			*path, err = config.ExpandPath(*path)
			if err != nil {
				return cliError(stderr, 2, "cannot expand configured path")
			}
		}
	}
	if !visited["cache-dir"] && *cacheDir != "" && !filepath.IsAbs(*cacheDir) {
		*cacheDir = filepath.Join(filepath.Dir(cfg.Path), *cacheDir)
	}
	if !visited["reflect"] && *reflectPath != "" && !filepath.IsAbs(*reflectPath) && (filepath.Ext(*reflectPath) == ".py" || filepath.Dir(*reflectPath) != ".") {
		*reflectPath = filepath.Join(filepath.Dir(cfg.Path), *reflectPath)
	}
	if *cacheDir == "" {
		*cacheDir, err = DefaultCacheDir()
		if err != nil {
			return cliError(stderr, 1, err.Error())
		}
	}
	if !labelPattern.MatchString(*provider) || Redact(*provider) != *provider {
		return cliError(stderr, 2, "invalid provider label")
	}
	if *dryRun {
		source := "reflect"
		if *input != "" {
			source = "normalized_json"
		}
		if *snapshot != "" {
			source = "snapshot"
		}
		return emitJSON(stdout, stderr, map[string]any{"action": "prepare_evidence", "source": source, "provider": *provider, "days": *days, "all_projects": *allProjects, "cache_enabled": !*noCache, "cache_ttl_seconds": cacheTTL.Seconds(), "max_output_bytes": *maxOutput, "model_calls": 0, "writes": false})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var bundle *Bundle
	var cache CacheInfo
	if *snapshot != "" {
		bundle, cache, err = ReadSnapshotInfo(*cacheDir, *snapshot)
		if err == nil && !visited["provider"] {
			*provider = cache.Provider
		}

	} else {
		fmt.Fprintln(stderr, "Preparing discovery evidence...")
		bundle, cache, err = Load(ctx, SourceOptions{Input: *input, Reflect: *reflectPath, Provider: *provider, Project: *project, Days: *days, AllProjects: *allProjects, CacheDir: *cacheDir, CacheTTL: *cacheTTL, Timeout: time.Duration(*timeout * float64(time.Second)), Refresh: *refresh, NoCache: *noCache, MaxInputBytes: opts.MaxInputBytes, MaxRecords: opts.MaxRecords, MaxRecordBytes: opts.MaxRecordBytes}, stdin)
	}
	if err != nil {
		return cliError(stderr, 1, Redact(err.Error()))
	}
	render := renderOptions{Offset: *offset, PageSize: *pageSize, MinSessions: *minSessions, TextOffset: *textOffset, TextLimit: *textLimit, MaxBytes: *maxOutput, Evidence: *evidence, AllRecords: *allRecords}
	render.CarryArgs = []string{"--provider", *provider}
	// Continuations contain only safe flags. The caller retains explicit local
	// path context; paths never become evidence or delegatable output.
	for _, name := range []string{"cache-dir", "project", "config", "criteria", "inventory"} {
		if visited[name] {
			render.RequiredContextFlags = append(render.RequiredContextFlags, "--"+name)
		}
	}

	if *evidence == "" && !*allRecords {
		render.Criteria, err = loadCriteria(*criteriaPath, *project)
		if err != nil {
			return cliError(stderr, 2, err.Error())
		}
		render.Inventory, err = LoadInventory(*provider, *project, *inventoryPath)
		if err != nil {
			return cliError(stderr, 2, Redact(err.Error()))
		}
	}
	data, err := renderBundle(bundle, cache, render)
	if err != nil {
		return cliError(stderr, 2, err.Error())
	}
	if _, err := stdout.Write(data); err != nil {
		return cliError(stderr, 1, "could not write discovery output")
	}
	return 0
}

func loadCriteria(path, project string) (string, error) {
	if path == "" {
		return Redact(string(assets.ToolCriteria)), nil
	}
	data, err := bounded.File(path, 16384, "criteria")

	if err != nil {
		return "", errors.New("cannot read criteria (maximum 16 KiB)")
	}
	return Redact(string(data)), nil
}

func emitJSON(stdout, stderr io.Writer, v any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(v); err != nil {
		return cliError(stderr, 1, "could not write JSON")
	}
	return 0
}
func cliError(stderr io.Writer, code int, message string) int {
	fmt.Fprintln(stderr, "error:", message)
	return code
}

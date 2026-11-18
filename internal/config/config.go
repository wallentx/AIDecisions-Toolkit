// Package config resolves shared, per-provider, and per-tool settings without persisting secrets.
package config

import (
	"aid/internal/bounded"
	"aid/internal/decisions"
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:embed default.yml
var DefaultYAML []byte

// ProviderSettings configures credentials, model defaults, and endpoint overrides for one provider.
type ProviderSettings struct {
	APIKeyFile string `yaml:"api_key_file"`
	Model      string `yaml:"model"`
	Endpoint   string `yaml:"endpoint"`
}

// Options are settings shared by AID tools. Tools may additionally decode their
// own section through DecodeTool without adding fields to this package.
type Options struct {
	Provider       string            `yaml:"provider"`
	APIKeyFile     string            `yaml:"api_key_file"`
	APIKeyFiles    map[string]string `yaml:"api_key_files"`
	Model          string            `yaml:"model"`
	Endpoint       string            `yaml:"endpoint"`
	TimeoutSeconds float64           `yaml:"timeout_seconds"`
	Output         string            `yaml:"output"`
	ChecksFile     string            `yaml:"checks_file"`
	Context        string            `yaml:"context"`
}

// Overrides carries explicit CLI flag values applied above tool, provider, and global config.
type Overrides struct {
	Provider       *string
	Model          *string
	Endpoint       *string
	APIKeyFile     *string
	ChecksFile     *string
	Context        *string
	TimeoutSeconds *float64
	Output         *string
}

type Updates struct {
	Repository string `yaml:"repository"`
	InstallDir string `yaml:"install_dir"`
}

type Config struct {
	Version   int                         `yaml:"version"`
	Global    Options                     `yaml:"global"`
	Providers map[string]ProviderSettings `yaml:"providers"`
	Updates   Updates                     `yaml:"updates"`
	Tools     map[string]yaml.Node        `yaml:"tools"`
	Path      string                      `yaml:"-"`
}

func DefaultPath() (string, error) {
	if path := os.Getenv("AID_CONFIG"); path != "" {
		return ExpandPath(path)
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "AID", "config.yml"), nil
}

func ExpandPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if len(path) <= 2 {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func resolveConfigPath(baseDir, raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	expanded, err := ExpandPath(raw)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(baseDir, expanded)
	}
	return expanded, nil
}

func Load(path string) (Config, error) {
	explicit := path != "" || os.Getenv("AID_CONFIG") != ""
	var err error
	if path == "" {
		path, err = DefaultPath()
	} else {
		path, err = ExpandPath(path)
	}
	if err != nil {
		return Config{}, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	data, err := bounded.File(path, 1<<20, "AID config")
	if errors.Is(err, os.ErrNotExist) && !explicit {
		data, err = DefaultYAML, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read AID config: %w", err)
	}
	if len(data) > 1<<20 {
		return Config{}, errors.New("AID config exceeds 1 MiB")
	}
	cfg := Config{
		Version: 1,
		Global: Options{
			TimeoutSeconds: 30,
			Output:         "auto",
		},
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("invalid AID config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("AID config must contain exactly one YAML document")
	}
	if cfg.Version != 1 {
		return Config{}, fmt.Errorf("unsupported AID config version %d", cfg.Version)
	}
	cfg.Path = path
	if err := validateGlobalAndProviders(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateGlobalAndProviders(cfg Config) error {
	if _, err := decisions.ParseProvider(cfg.Global.Provider); err != nil {
		return fmt.Errorf("global.provider: %w", err)
	}
	if err := validateKeyFilesMap("global.api_key_files", cfg.Global.APIKeyFiles); err != nil {
		return err
	}
	for name, settings := range cfg.Providers {
		if strings.TrimSpace(name) == "" {
			return errors.New("providers key must be nonempty")
		}
		if _, err := decisions.ParseProvider(name); err != nil {
			return fmt.Errorf("providers.%s: %w", name, err)
		}
		if settings.Endpoint != "" {
			if _, err := decisions.ValidateEndpoint(settings.Endpoint); err != nil {
				return fmt.Errorf("providers.%s.endpoint: %w", name, err)
			}
		}
	}
	if cfg.Global.Endpoint != "" {
		if _, err := decisions.ValidateEndpoint(cfg.Global.Endpoint); err != nil {
			return fmt.Errorf("global.endpoint: %w", err)
		}
	}
	return validate(cfg.Global)
}

func validateKeyFilesMap(scope string, files map[string]string) error {
	for name := range files {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s: provider name must be nonempty", scope)
		}
		if _, err := decisions.ParseProvider(name); err != nil {
			return fmt.Errorf("%s.%s: %w", scope, name, err)
		}
	}
	return nil
}

func validate(opts Options) error {
	if !math.IsNaN(opts.TimeoutSeconds) && !math.IsInf(opts.TimeoutSeconds, 0) && opts.TimeoutSeconds > 0 && opts.TimeoutSeconds <= 3600 {
		switch opts.Output {
		case "auto", "json", "text":
			return nil
		}
		return errors.New("output must be auto, json, or text")
	}
	return errors.New("timeout_seconds must be greater than 0 and at most 3600")
}

// DecodeTool decodes only a tool's private YAML section into its own schema.
func (cfg Config) DecodeTool(name string, out any) error {
	node, ok := cfg.Tools[name]
	if !ok {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("tools.%s must be a mapping", name)
	}
	return node.Decode(out)
}

type commonToolLayer struct {
	APIKeyFile     *string  `yaml:"api_key_file"`
	Model          *string  `yaml:"model"`
	TimeoutSeconds *float64 `yaml:"timeout_seconds"`
	Output         *string  `yaml:"output"`
	ChecksFile     *string  `yaml:"checks_file"`
	Context        *string  `yaml:"context"`
}

// Resolve merges shared global and per-tool settings without interpreting private
// tool fields (for example tools.discover.provider). Decisions-backed tools should
// call ResolveDecisions to resolve provider selection, endpoints, and isolated keys.
func (cfg Config) Resolve(name string) (Options, error) {
	globalProvider, err := decisions.ParseProvider(cfg.Global.Provider)
	if err != nil {
		return Options{}, fmt.Errorf("global.provider: %w", err)
	}
	info, _ := decisions.LookupProvider(globalProvider)

	opts := cfg.Global
	opts.Provider = string(globalProvider)
	if globalProvider != decisions.DefaultProvider {
		opts.APIKeyFile = ""
	}
	if strings.TrimSpace(opts.Model) == "" {
		opts.Model = info.DefaultModel
	}
	if strings.TrimSpace(opts.Endpoint) == "" {
		opts.Endpoint = info.DefaultEndpoint
	}

	var layer commonToolLayer
	if err := cfg.DecodeTool(name, &layer); err != nil {
		return Options{}, err
	}
	if layer.APIKeyFile != nil {
		opts.APIKeyFile = *layer.APIKeyFile
	}
	if layer.Model != nil {
		opts.Model = *layer.Model
	}
	if layer.TimeoutSeconds != nil {
		opts.TimeoutSeconds = *layer.TimeoutSeconds
	}
	if layer.Output != nil {
		opts.Output = *layer.Output
	}
	if layer.ChecksFile != nil {
		opts.ChecksFile = *layer.ChecksFile
	}
	if layer.Context != nil {
		opts.Context = *layer.Context
	}
	if err := validate(opts); err != nil {
		return Options{}, fmt.Errorf("tools.%s: %w", name, err)
	}
	baseDir := filepath.Dir(cfg.Path)
	for _, field := range []*string{&opts.APIKeyFile, &opts.ChecksFile} {
		if *field == "" {
			continue
		}
		resolved, err := resolveConfigPath(baseDir, *field)
		if err != nil {
			return Options{}, err
		}
		*field = resolved
	}
	return opts, nil
}

type toolOptionsLayer struct {
	Provider       *string           `yaml:"provider"`
	APIKeyFile     *string           `yaml:"api_key_file"`
	APIKeyFiles    map[string]string `yaml:"api_key_files"`
	Model          *string           `yaml:"model"`
	Endpoint       *string           `yaml:"endpoint"`
	TimeoutSeconds *float64          `yaml:"timeout_seconds"`
	Output         *string           `yaml:"output"`
	ChecksFile     *string           `yaml:"checks_file"`
	Context        *string           `yaml:"context"`
}

// ResolveDecisions resolves effective provider, model, endpoint, isolated credentials,
// and shared options across global, provider, tool, and CLI layers.
func (cfg Config) ResolveDecisions(name string, overrides Overrides) (Options, error) {
	globalProvider, err := decisions.ParseProvider(cfg.Global.Provider)
	if err != nil {
		return Options{}, fmt.Errorf("global.provider: %w", err)
	}
	if err := validateKeyFilesMap("global.api_key_files", cfg.Global.APIKeyFiles); err != nil {
		return Options{}, err
	}

	var toolLayer toolOptionsLayer
	if err := cfg.DecodeTool(name, &toolLayer); err != nil {
		return Options{}, err
	}
	if err := validateKeyFilesMap("tools."+name+".api_key_files", toolLayer.APIKeyFiles); err != nil {
		return Options{}, err
	}

	toolProvider := globalProvider
	if toolLayer.Provider != nil {
		if strings.TrimSpace(*toolLayer.Provider) == "" {
			return Options{}, fmt.Errorf("tools.%s.provider: provider must be nonempty", name)
		}
		toolProvider, err = decisions.ParseProvider(*toolLayer.Provider)
		if err != nil {
			return Options{}, fmt.Errorf("tools.%s.provider: %w", name, err)
		}
	}

	effectiveProvider := toolProvider
	if overrides.Provider != nil {
		if strings.TrimSpace(*overrides.Provider) == "" {
			return Options{}, errors.New("provider must be nonempty")
		}
		effectiveProvider, err = decisions.ParseProvider(*overrides.Provider)
		if err != nil {
			return Options{}, err
		}
	}

	providerKey := string(effectiveProvider)
	providerCfg := cfg.Providers[providerKey]
	providerInfo, _ := decisions.LookupProvider(effectiveProvider)

	// Model precedence for effectiveProvider:
	// CLI --model > tools.<name>.model (if tool targets effectiveProvider) >
	// providers.<provider>.model > global.model (if global targets effectiveProvider) >
	// provider built-in default.
	model := providerInfo.DefaultModel
	if globalProvider == effectiveProvider && strings.TrimSpace(cfg.Global.Model) != "" {
		model = strings.TrimSpace(cfg.Global.Model)
	}
	if strings.TrimSpace(providerCfg.Model) != "" {
		model = strings.TrimSpace(providerCfg.Model)
	}
	if toolProvider == effectiveProvider && toolLayer.Model != nil && strings.TrimSpace(*toolLayer.Model) != "" {
		model = strings.TrimSpace(*toolLayer.Model)
	}
	if overrides.Model != nil {
		if strings.TrimSpace(*overrides.Model) == "" {
			return Options{}, errors.New("model must be nonempty")
		}
		model = strings.TrimSpace(*overrides.Model)
	}

	// Endpoint precedence for effectiveProvider (never inherited across different providers):
	endpoint := providerInfo.DefaultEndpoint
	if globalProvider == effectiveProvider && strings.TrimSpace(cfg.Global.Endpoint) != "" {
		endpoint = strings.TrimSpace(cfg.Global.Endpoint)
	}
	if strings.TrimSpace(providerCfg.Endpoint) != "" {
		endpoint = strings.TrimSpace(providerCfg.Endpoint)
	}
	if toolProvider == effectiveProvider && toolLayer.Endpoint != nil && strings.TrimSpace(*toolLayer.Endpoint) != "" {
		endpoint = strings.TrimSpace(*toolLayer.Endpoint)
	}
	if overrides.Endpoint != nil {
		if strings.TrimSpace(*overrides.Endpoint) == "" {
			return Options{}, errors.New("endpoint must be nonempty")
		}
		endpoint = strings.TrimSpace(*overrides.Endpoint)
	}

	baseDir := filepath.Dir(cfg.Path)

	// Isolated APIKeyFile resolution for effectiveProvider:
	// Legacy global.api_key_file applies ONLY to OpenAI (DefaultProvider).
	// Non-OpenAI providers use providers.<id>.api_key_file, api_key_files.<id>,
	// tools.<name>.api_key_file (when targeting that provider), or --api-key-file.
	keyFile := ""
	if effectiveProvider == decisions.DefaultProvider && cfg.Global.APIKeyFile != "" {
		keyFile, err = resolveConfigPath(baseDir, cfg.Global.APIKeyFile)
		if err != nil {
			return Options{}, err
		}
	}
	if pFile := cfg.Global.APIKeyFiles[providerKey]; pFile != "" {
		keyFile, err = resolveConfigPath(baseDir, pFile)
		if err != nil {
			return Options{}, err
		}
	}
	if providerCfg.APIKeyFile != "" {
		keyFile, err = resolveConfigPath(baseDir, providerCfg.APIKeyFile)
		if err != nil {
			return Options{}, err
		}
	}
	if toolProvider == effectiveProvider && toolLayer.APIKeyFile != nil {
		keyFile, err = resolveConfigPath(baseDir, *toolLayer.APIKeyFile)
		if err != nil {
			return Options{}, err
		}
	}
	if pFile, ok := toolLayer.APIKeyFiles[providerKey]; ok {
		keyFile, err = resolveConfigPath(baseDir, pFile)
		if err != nil {
			return Options{}, err
		}
	}
	if overrides.APIKeyFile != nil {
		if strings.TrimSpace(*overrides.APIKeyFile) == "" {
			return Options{}, errors.New("api-key-file must be nonempty")
		}
		expanded, expandErr := ExpandPath(*overrides.APIKeyFile)
		if expandErr != nil {
			return Options{}, expandErr
		}
		absKey, absErr := filepath.Abs(expanded)
		if absErr != nil {
			return Options{}, absErr
		}
		keyFile = absKey
	}

	opts := Options{
		Provider:       providerKey,
		APIKeyFile:     keyFile,
		Model:          model,
		Endpoint:       endpoint,
		TimeoutSeconds: cfg.Global.TimeoutSeconds,
		Output:         cfg.Global.Output,
		ChecksFile:     cfg.Global.ChecksFile,
		Context:        cfg.Global.Context,
	}
	if toolLayer.TimeoutSeconds != nil {
		opts.TimeoutSeconds = *toolLayer.TimeoutSeconds
	}
	if toolLayer.Output != nil {
		opts.Output = *toolLayer.Output
	}
	if toolLayer.ChecksFile != nil {
		opts.ChecksFile = *toolLayer.ChecksFile
	}
	if toolLayer.Context != nil {
		opts.Context = *toolLayer.Context
	}
	if opts.ChecksFile != "" {
		opts.ChecksFile, err = resolveConfigPath(baseDir, opts.ChecksFile)
		if err != nil {
			return Options{}, err
		}
	}

	if overrides.ChecksFile != nil {
		expanded, expandErr := ExpandPath(*overrides.ChecksFile)
		if expandErr != nil {
			return Options{}, expandErr
		}
		opts.ChecksFile = expanded
	}
	if overrides.Context != nil {
		opts.Context = *overrides.Context
	}
	if overrides.TimeoutSeconds != nil {
		opts.TimeoutSeconds = *overrides.TimeoutSeconds
	}
	if overrides.Output != nil {
		opts.Output = *overrides.Output
	}
	if opts.Output == "table" {
		opts.Output = "text"
	}

	if err := validate(opts); err != nil {
		if overrides.TimeoutSeconds != nil || overrides.Output != nil {
			return Options{}, err
		}
		return Options{}, fmt.Errorf("tools.%s: %w", name, err)
	}
	if opts.Endpoint != "" {
		if _, err := decisions.ValidateEndpoint(opts.Endpoint); err != nil {
			return Options{}, fmt.Errorf("endpoint: %w", err)
		}
	}
	return opts, nil
}

// APIKey resolves the credential for opts.Provider from that provider's own
// environment variable or isolated key file. It never includes key bytes in errors.
func APIKey(opts Options) (string, error) {
	provider, err := decisions.ParseProvider(opts.Provider)
	if err != nil {
		return "", err
	}
	info, _ := decisions.LookupProvider(provider)
	key := strings.TrimSpace(os.Getenv(info.APIKeyEnv))
	if key == "" && opts.APIKeyFile != "" {
		path, err := ExpandPath(opts.APIKeyFile)
		if err != nil {
			return "", err
		}
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("read api_key_file: %w", err)
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 16385))
		if err != nil {
			return "", errors.New("could not read api_key_file")
		}
		if len(data) > 16384 {
			return "", errors.New("api_key_file exceeds 16 KiB")
		}
		key = strings.TrimSpace(string(data))
	}
	if key == "" {
		if provider == decisions.DefaultProvider {
			return "", fmt.Errorf("set %s or providers.%s.api_key_file (or global.api_key_file) in AID config; --dry-run needs no key", info.APIKeyEnv, provider)
		}
		return "", fmt.Errorf("set %s or providers.%s.api_key_file in AID config; --dry-run needs no key", info.APIKeyEnv, provider)
	}
	if err := decisions.ValidateAPIKey(key, provider); err != nil {
		return "", err
	}
	return key, nil
}

// EnsureDefault creates a private default file only when none exists.
func EnsureDefault(path string) error {
	var err error
	if path == "" {
		path, err = DefaultPath()
	} else {
		path, err = ExpandPath(path)
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, writeErr := file.Write(DefaultYAML)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

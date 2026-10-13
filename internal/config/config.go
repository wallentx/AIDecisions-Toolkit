// Package config resolves shared and per-tool settings without persisting secrets.
package config

import (
	"aid/internal/bounded"
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

// Options are settings shared by AID tools. Tools may additionally decode their
// own section through DecodeTool without adding fields to this package.
type Options struct {
	APIKeyFile     string  `yaml:"api_key_file"`
	Model          string  `yaml:"model"`
	TimeoutSeconds float64 `yaml:"timeout_seconds"`
	Output         string  `yaml:"output"`
	ChecksFile     string  `yaml:"checks_file"`
	Context        string  `yaml:"context"`
}

type Updates struct {
	Repository string `yaml:"repository"`
	InstallDir string `yaml:"install_dir"`
}

type Config struct {
	Version int                  `yaml:"version"`
	Global  Options              `yaml:"global"`
	Updates Updates              `yaml:"updates"`
	Tools   map[string]yaml.Node `yaml:"tools"`
	Path    string               `yaml:"-"`
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
	cfg := Config{Version: 1, Global: Options{Model: "gpt-6-luna", TimeoutSeconds: 30, Output: "auto"}}
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
	if err := validate(cfg.Global); err != nil {
		return Config{}, err
	}
	return cfg, nil
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

func (cfg Config) Resolve(name string) (Options, error) {
	opts := cfg.Global
	if err := cfg.DecodeTool(name, &opts); err != nil {
		return Options{}, err
	}
	if err := validate(opts); err != nil {
		return Options{}, fmt.Errorf("tools.%s: %w", name, err)
	}
	for _, path := range []*string{&opts.APIKeyFile, &opts.ChecksFile} {
		if *path == "" {
			continue
		}
		expanded, err := ExpandPath(*path)
		if err != nil {
			return Options{}, err
		}
		if !filepath.IsAbs(expanded) {
			expanded = filepath.Join(filepath.Dir(cfg.Path), expanded)
		}
		*path = expanded
	}
	return opts, nil
}

// APIKey prefers the process environment. It never includes key bytes in errors.
func APIKey(opts Options) (string, error) {
	key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
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
		return "", errors.New("set OPENAI_API_KEY or global.api_key_file in AID config; --dry-run needs no key")
	}
	for _, r := range key {
		if r <= 32 || r >= 127 {
			return "", errors.New("API key must contain printable ASCII without whitespace")
		}
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

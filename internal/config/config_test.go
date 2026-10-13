package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayeringAndRelativePaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	data := `version: 1
global:
  model: gpt-6-luna
  timeout_seconds: 30
  output: auto
  api_key_file: key
tools:
  diff:
    timeout_seconds: 12
    context: public API
    checks_file: checks.json
    private_setting: true
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := cfg.Resolve("diff")
	if err != nil {
		t.Fatal(err)
	}
	if opts.TimeoutSeconds != 12 || opts.Model != "gpt-6-luna" || opts.Context != "public API" || opts.APIKeyFile != filepath.Join(filepath.Dir(path), "key") || opts.ChecksFile != filepath.Join(filepath.Dir(path), "checks.json") {
		t.Fatalf("bad merge: %+v", opts)
	}
}

func TestInvalidConfig(t *testing.T) {
	for _, data := range []string{"version: 2", "global:\n  timeout_seconds: 0", "global:\n  output: bad", "global:\n  typo: x", "version: 1\n---\nversion: 1", "version: 1\nversion: 1"} {
		path := filepath.Join(t.TempDir(), "config.yml")
		os.WriteFile(path, []byte(data), 0600)
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted invalid config: %s", data)
		}
	}
}

func TestKeyPrecedenceAndRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	os.WriteFile(path, []byte("file-key\n"), 0600)
	t.Setenv("OPENAI_API_KEY", "env-key")
	key, err := APIKey(Options{APIKeyFile: path})
	if err != nil || key != "env-key" {
		t.Fatalf("%q %v", key, err)
	}
	t.Setenv("OPENAI_API_KEY", "")
	key, err = APIKey(Options{APIKeyFile: path})
	if err != nil || key != "file-key" {
		t.Fatalf("%q %v", key, err)
	}
	os.WriteFile(path, []byte("private\nsecret"), 0600)
	_, err = APIKey(Options{APIKeyFile: path})
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe error %v", err)
	}
}

func TestCreatePreservesExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AID", "config.yml")
	if err := EnsureDefault(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("custom"), 0600)
	if err := EnsureDefault(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "custom" {
		t.Fatal("overwrote existing config")
	}
}

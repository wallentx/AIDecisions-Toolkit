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

func TestDefaultConfigCompatibleWithNonDecisionTools(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := EnsureDefault(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// tools.discover.provider is "codex" (a history source, not a Decisions provider).
	if _, err := cfg.Resolve("discover"); err != nil {
		t.Fatalf("Resolve(discover) rejected DefaultYAML: %v", err)
	}
	diffOpts, err := cfg.ResolveDecisions("diff", Overrides{})
	if err != nil {
		t.Fatalf("ResolveDecisions(diff): %v", err)
	}
	if diffOpts.Provider != "openai" || diffOpts.Model != "gpt-6-luna" {
		t.Fatalf("unexpected default diff options: %+v", diffOpts)
	}
}

func TestProviderPrecedenceAndCrossProviderIsolation(t *testing.T) {
	dir := t.TempDir()
	openAIKey := filepath.Join(dir, "openai.key")
	geminiKey := filepath.Join(dir, "gemini.key")
	os.WriteFile(openAIKey, []byte("openai-file-token\n"), 0600)
	os.WriteFile(geminiKey, []byte("gemini-file-token\n"), 0600)

	cfgPath := filepath.Join(dir, "config.yml")
	data := `version: 1
global:
  provider: openai
  model: gpt-6-luna
  endpoint: https://api.openai.com/v1/decisions
  api_key_file: openai.key
providers:
  gemini:
    api_key_file: gemini.key
`
	if err := os.WriteFile(cfgPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Default resolves to OpenAI with OpenAI's key file even if GEMINI_API_KEY is set.
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "gemini-env-token")
	t.Setenv("TYPESAFE_API_KEY", "")
	openAIOpts, err := cfg.ResolveDecisions("diff", Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if openAIOpts.Provider != "openai" || openAIOpts.Model != "gpt-6-luna" || openAIOpts.APIKeyFile != openAIKey {
		t.Fatalf("unexpected OpenAI opts: %+v", openAIOpts)
	}
	gotKey, err := APIKey(openAIOpts)
	if err != nil || gotKey != "openai-file-token" {
		t.Fatalf("OpenAI should ignore GEMINI_API_KEY: got %q, %v", gotKey, err)
	}

	// 2. Switching --provider to typesafe must NOT inherit OpenAI's model, endpoint, or api_key_file.
	tsProvider := "typesafe"
	tsOpts, err := cfg.ResolveDecisions("diff", Overrides{Provider: &tsProvider})
	if err != nil {
		t.Fatal(err)
	}
	if tsOpts.Provider != "typesafe" || tsOpts.Model != "jev-latest" || tsOpts.Endpoint != "https://api.typesafe.ai/v1/systemone" || tsOpts.APIKeyFile != "" {
		t.Fatalf("OpenAI settings leaked into typesafe: %+v", tsOpts)
	}
	t.Setenv("OPENAI_API_KEY", "openai-env-token")
	if _, err := APIKey(tsOpts); err == nil || !strings.Contains(err.Error(), "TYPESAFE_API_KEY") {
		t.Fatalf("typesafe accepted missing TYPESAFE_API_KEY when OPENAI_API_KEY was set: %v", err)
	}

	// 3. Switching --provider to gemini uses Gemini's default model/endpoint and providers.gemini.api_key_file.
	gemProvider := "gemini"
	gemOpts, err := cfg.ResolveDecisions("diff", Overrides{Provider: &gemProvider})
	if err != nil {
		t.Fatal(err)
	}
	if gemOpts.Provider != "gemini" || gemOpts.Model != "gemini-decision-1" || gemOpts.Endpoint != "https://generativelanguage.googleapis.com/v1beta/decisions" || gemOpts.APIKeyFile != geminiKey {
		t.Fatalf("unexpected Gemini opts: %+v", gemOpts)
	}
	// Environment variable GEMINI_API_KEY takes precedence over providers.gemini.api_key_file.
	gotGemKey, err := APIKey(gemOpts)
	if err != nil || gotGemKey != "gemini-env-token" {
		t.Fatalf("expected gemini-env-token, got %q (%v)", gotGemKey, err)
	}
	t.Setenv("GEMINI_API_KEY", "")
	gotGemKey, err = APIKey(gemOpts)
	if err != nil || gotGemKey != "gemini-file-token" {
		t.Fatalf("expected gemini-file-token, got %q (%v)", gotGemKey, err)
	}
	// 4. Switching --provider to openrouter uses OpenRouter's default model/endpoint and isolated OPENROUTER_API_KEY.
	orProvider := "openrouter"
	orOpts, err := cfg.ResolveDecisions("diff", Overrides{Provider: &orProvider})
	if err != nil {
		t.Fatal(err)
	}
	if orOpts.Provider != "openrouter" || orOpts.Model != "~typesafe/jev-latest" || orOpts.Endpoint != "https://openrouter.ai/api/alpha/decisions" || orOpts.APIKeyFile != "" {
		t.Fatalf("unexpected OpenRouter opts: %+v", orOpts)
	}
	t.Setenv("OPENROUTER_API_KEY", "")
	if _, err := APIKey(orOpts); err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Fatalf("openrouter accepted missing OPENROUTER_API_KEY when OPENAI_API_KEY was set: %v", err)
	}
	t.Setenv("OPENROUTER_API_KEY", "openrouter-env-token")
	gotORKey, err := APIKey(orOpts)
	if err != nil || gotORKey != "openrouter-env-token" {
		t.Fatalf("expected openrouter-env-token, got %q (%v)", gotORKey, err)
	}

	// 5. Explicit empty tools.diff.api_key_file: "" clears global.api_key_file fallback.
	clearCfgPath := filepath.Join(dir, "clear-key.yml")
	clearData := `version: 1
global:
  api_key_file: openai.key
tools:
  diff:
    api_key_file: ""
`
	if err := os.WriteFile(clearCfgPath, []byte(clearData), 0600); err != nil {
		t.Fatal(err)
	}
	clearCfg, err := Load(clearCfgPath)
	if err != nil {
		t.Fatal(err)
	}
	clearedOpts, err := clearCfg.ResolveDecisions("diff", Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if clearedOpts.APIKeyFile != "" {
		t.Fatalf("expected tools.diff.api_key_file: \"\" to clear global key file, got %q", clearedOpts.APIKeyFile)
	}
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := APIKey(clearedOpts); err == nil {
		t.Fatal("expected missing OPENAI_API_KEY error when tools.diff.api_key_file disabled global key file")
	}
}

func TestInvalidConfig(t *testing.T) {
	cases := []string{
		"version: 2",
		"global:\n  timeout_seconds: 0",
		"global:\n  output: bad",
		"global:\n  typo: x",
		"global:\n  provider: anthropic",
		"global:\n  endpoint: ftp://bad.example.com",
		"providers:\n  unknown:\n    model: x",
		"providers:\n  litellm:\n    endpoint: https://user:secret@gateway.example.com",
		"version: 1\n---\nversion: 1",
		"version: 1\nversion: 1",
	}
	for _, data := range cases {
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

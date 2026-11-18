package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aid/internal/buildinfo"
	"aid/internal/decisions"
)

func TestDryRunConfigAndFlagPrecedence(t *testing.T) {
	dir := t.TempDir()
	checks := filepath.Join(dir, "checks.json")
	writeFile(t, checks, `{"custom":"Does this change behavior?"}`)
	cfg := filepath.Join(dir, "config.yml")
	writeFile(t, cfg, fmt.Sprintf(`global:
  model: global-model
  context: global context
  timeout_seconds: 7
  output: text
  api_key_file: %q
tools:
  diff:
    model: tool-model
    context: tool context
    checks_file: %q
`, filepath.Join(dir, "absent-key"), checks))
	t.Setenv("OPENAI_API_KEY", "invalid key never read")
	for _, test := range []struct {
		name         string
		args         []string
		wantProvider decisions.Provider
		wantEndpoint string
		wantModel    string
		wantContext  string
	}{
		{"tool", nil, decisions.ProviderOpenAI, decisions.OpenAIEndpoint, "tool-model", "tool context"},
		{"flags", []string{"--model", "flag-model", "--context", ""}, decisions.ProviderOpenAI, decisions.OpenAIEndpoint, "flag-model", ""},
		{"switch_to_typesafe", []string{"--provider", "typesafe"}, decisions.ProviderTypeSafe, decisions.TypeSafeEndpoint, decisions.TypeSafeDefaultModel, "tool context"},
		{"switch_to_gemini", []string{"--provider", "gemini"}, decisions.ProviderGemini, decisions.GeminiEndpoint, decisions.GeminiDefaultModel, "tool context"},
		{"switch_to_openrouter", []string{"--provider", "openrouter"}, decisions.ProviderOpenRouter, decisions.OpenRouterEndpoint, decisions.OpenRouterDefaultModel, "tool context"},
		{"switch_to_litellm", []string{"--provider", "litellm", "--model", "jev", "--endpoint", "http://127.0.0.1:4000"}, decisions.ProviderLiteLLM, "http://127.0.0.1:4000/v1/decisions", "jev", "tool context"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"--config", cfg, "--example", "--dry-run"}, test.args...)
			var out, stderr bytes.Buffer
			if code := Run(args, strings.NewReader(""), &out, &stderr); code != 0 {
				t.Fatalf("code %d: %s", code, &stderr)
			}
			var preview decisions.Preview
			if err := json.Unmarshal(out.Bytes(), &preview); err != nil {
				t.Fatal(err)
			}
			if preview.Provider != test.wantProvider || preview.Endpoint != test.wantEndpoint || preview.Model != test.wantModel {
				t.Fatalf("unexpected preview header: %+v", preview)
			}

			switch test.wantProvider {
			case decisions.ProviderOpenAI, decisions.ProviderLiteLLM:
				var wire struct {
					Model     string               `json:"model"`
					Input     string               `json:"input"`
					Questions []decisions.Question `json:"questions"`
				}
				if err := json.Unmarshal(preview.Request, &wire); err != nil {
					t.Fatal(err)
				}
				var input map[string]string
				if err := json.Unmarshal([]byte(wire.Input), &input); err != nil {
					t.Fatal(err)
				}
				if wire.Model != test.wantModel || input["context"] != test.wantContext || len(wire.Questions) != 1 || wire.Questions[0].Name != "custom" || wire.Questions[0].Type != "predicate" {
					t.Fatalf("unexpected OpenAI/LiteLLM wire request: %+v, %+v", wire, input)
				}
			case decisions.ProviderTypeSafe, decisions.ProviderOpenRouter:
				var wire struct {
					Model     string                    `json:"model"`
					State     string                    `json:"state"`
					Questions map[string]map[string]any `json:"questions"`
				}
				if err := json.Unmarshal(preview.Request, &wire); err != nil {
					t.Fatal(err)
				}
				var state map[string]string
				if err := json.Unmarshal([]byte(wire.State), &state); err != nil {
					t.Fatal(err)
				}
				if wire.Model != test.wantModel || state["context"] != test.wantContext || wire.Questions["custom"]["type"] != "noul" {
					t.Fatalf("unexpected TypeSafe/OpenRouter wire request: %+v", wire)
				}
			case decisions.ProviderGemini:
				var wire struct {
					Model     string                    `json:"model"`
					Input     string                    `json:"input"`
					Questions map[string]map[string]any `json:"questions"`
				}
				if err := json.Unmarshal(preview.Request, &wire); err != nil {
					t.Fatal(err)
				}
				var input map[string]string
				if err := json.Unmarshal([]byte(wire.Input), &input); err != nil {
					t.Fatal(err)
				}
				if wire.Model != test.wantModel || input["context"] != test.wantContext || wire.Questions["custom"]["type"] != "noul" {
					t.Fatalf("unexpected Gemini wire request: %+v", wire)
				}
			}
		})
	}
}

func TestLiveCredentialOutputAndRefusal(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	writeFile(t, keyPath, "file-key\n")
	cfg := filepath.Join(dir, "config.yml")
	writeFile(t, cfg, fmt.Sprintf("global:\n  api_key_file: %q\n", keyPath))
	for _, test := range []struct {
		name     string
		envKey   string
		wantKey  string
		args     []string
		refusal  bool
		wantCode int
		wantJSON bool
	}{
		{"file and automatic JSON", "", "file-key", nil, false, 0, true},
		{"environment wins", "env-key", "env-key", []string{"--output", "table"}, false, 0, false},
		{"refusal remains JSON", "", "file-key", []string{"--json"}, true, 3, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("OPENAI_API_KEY", test.envKey)
			calls := 0
			transport := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != decisions.Endpoint || r.Header.Get("Authorization") != "Bearer "+test.wantKey {
					t.Error("unexpected endpoint or credential")
				}
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > 2*time.Second {
					t.Error("explicit timeout not applied")
				}
				var request decisions.Request
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				answers := make([]decisions.Answer, 0, len(request.Questions))
				p := 0.75
				for i, question := range request.Questions {
					answer := decisions.Answer{Name: question.Name, Type: decisions.AnswerPredicate, Probability: &p}
					if test.refusal && i == 0 {
						answer.Type, answer.Probability = decisions.AnswerRefusal, nil
					}
					answers = append(answers, answer)
				}
				body, err := json.Marshal(map[string]any{"answers": answers})
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			})}
			args := append([]string{"--config", cfg, "--example", "--timeout", "2"}, test.args...)
			var out, stderr bytes.Buffer
			code := run(args, strings.NewReader(""), &out, &stderr, transport)
			if code != test.wantCode || calls != 1 {
				t.Fatalf("code=%d, calls=%d, stderr=%s", code, calls, &stderr)
			}
			if test.wantJSON {
				var answers map[string]decisions.Answer
				if err := json.Unmarshal(out.Bytes(), &answers); err != nil || len(answers) != 3 {
					t.Fatalf("invalid JSON result %s: %v", &out, err)
				}
			} else if !strings.Contains(out.String(), "PROBABILITY") || !strings.Contains(out.String(), "75.0%") {
				t.Fatalf("missing table: %s", &out)
			}
			if strings.Contains(out.String()+stderr.String(), test.wantKey) {
				t.Fatal("credential leaked into output")
			}
		})
	}
}

func TestLiveMultiProviderRoutingAndCredentialIsolation(t *testing.T) {
	dir := t.TempDir()
	openAIKeyFile := filepath.Join(dir, "openai.key")
	writeFile(t, openAIKeyFile, "openai-secret-file\n")
	cfg := filepath.Join(dir, "config.yml")
	writeFile(t, cfg, fmt.Sprintf(`global:
  api_key_file: %q
providers:
  litellm:
    endpoint: https://gateway.example.test
`, openAIKeyFile))

	t.Setenv("OPENAI_API_KEY", "openai-secret-env")
	t.Setenv("TYPESAFE_API_KEY", "typesafe-secret-env")
	t.Setenv("GEMINI_API_KEY", "gemini-secret-env")
	t.Setenv("OPENROUTER_API_KEY", "openrouter-secret-env")
	t.Setenv("LITELLM_API_KEY", "litellm-secret-env")

	cases := []struct {
		provider   string
		extraArgs  []string
		wantURL    string
		authHeader string
		wantAuth   string
		respBody   string
	}{
		{
			provider:   "typesafe",
			wantURL:    "https://api.typesafe.ai/v1/systemone",
			authHeader: "Authorization",
			wantAuth:   "Bearer typesafe-secret-env",
			respBody: `{
				"model": "jev-1.14.0",
				"answers": {
					"breaks_compatibility": {"type": "noul", "noul": 0.1},
					"changes_behavior": {"type": "noul", "noul": 0.8},
					"security_sensitive": {"type": "noul", "noul": 0.2}
				}
			}`,
		},
		{
			provider:   "gemini",
			wantURL:    "https://generativelanguage.googleapis.com/v1beta/decisions",
			authHeader: "x-goog-api-key",
			wantAuth:   "gemini-secret-env",
			respBody: `{
				"answers": {
					"breaks_compatibility": {"type": "noul", "noul": 0.1, "confidence": 0.9},
					"changes_behavior": {"type": "noul", "noul": 0.8, "confidence": 0.8},
					"security_sensitive": {"type": "noul", "noul": 0.2, "confidence": 0.8}
				}
			}`,
		},
		{
			provider:   "openrouter",
			extraArgs:  []string{"--model", "google/gemini-decision-1"},
			wantURL:    "https://openrouter.ai/api/alpha/decisions",
			authHeader: "Authorization",
			wantAuth:   "Bearer openrouter-secret-env",
			respBody: `{
				"model": "google/gemini-decision-1",
				"answers": {
					"breaks_compatibility": {"type": "noul", "noul": 0.1, "confidence": 0.9},
					"changes_behavior": {"type": "noul", "noul": 0.8, "confidence": 0.8},
					"security_sensitive": {"type": "noul", "noul": 0.2, "confidence": 0.8}
				}
			}`,
		},
		{
			provider:   "litellm",
			extraArgs:  []string{"--model", "jev"},
			wantURL:    "https://gateway.example.test/v1/decisions",
			authHeader: "Authorization",
			wantAuth:   "Bearer litellm-secret-env",
			respBody: `{
				"model": "typesafe/jev-1.14.0",
				"answers": [
					{"name": "breaks_compatibility", "type": "predicate", "probability": 0.1},
					{"name": "changes_behavior", "type": "predicate", "probability": 0.8},
					{"name": "security_sensitive", "type": "predicate", "probability": 0.2}
				]
			}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			transport := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != tc.wantURL {
					t.Errorf("got URL %q, want %q", r.URL.String(), tc.wantURL)
				}
				if got := r.Header.Get(tc.authHeader); got != tc.wantAuth {
					t.Errorf("got %s=%q, want %q", tc.authHeader, got, tc.wantAuth)
				}
				if tc.authHeader == "x-goog-api-key" && r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected Authorization header on Gemini request")
				}
				return &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(tc.respBody)),
					Header:     make(http.Header),
				}, nil
			})}
			args := append([]string{"--config", cfg, "--example", "--provider", tc.provider}, tc.extraArgs...)
			var out, stderr bytes.Buffer
			code := run(args, strings.NewReader(""), &out, &stderr, transport)
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, &stderr)
			}
			var answers map[string]decisions.Answer
			if err := json.Unmarshal(out.Bytes(), &answers); err != nil {
				t.Fatal(err)
			}
			if *answers["changes_behavior"].Probability != 0.8 || answers["changes_behavior"].Type != decisions.AnswerPredicate {
				t.Fatalf("unexpected normalized answer: %+v", answers["changes_behavior"])
			}
		})
	}

	// Missing GEMINI_API_KEY must fail with exit 1 and NEVER fall back to OPENAI_API_KEY or global.api_key_file.
	t.Setenv("GEMINI_API_KEY", "")
	var out, stderr bytes.Buffer
	code := Run([]string{"--config", cfg, "--example", "--provider", "gemini"}, strings.NewReader(""), &out, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "GEMINI_API_KEY") || strings.Contains(stderr.String(), "openai-secret") {
		t.Fatalf("expected missing GEMINI_API_KEY exit 1, got code=%d stderr=%s", code, &stderr)
	}
}

func TestVersionDoesNotNeedConfig(t *testing.T) {
	original := ManifestJSON
	t.Cleanup(func() { ManifestJSON = original })
	var manifest map[string]any
	if err := json.Unmarshal(ManifestJSON, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["version"] = "7.8.9"
	var err error
	ManifestJSON, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := Run([]string{"--config", "missing.yml", "--version", "--json"}, strings.NewReader(""), &out, &stderr)
	var version map[string]string
	if err := json.Unmarshal(out.Bytes(), &version); err != nil {
		t.Fatal(err)
	}
	if code != 0 || version["command"] != "AID.diff" || version["tool_version"] != "7.8.9" || version["release_version"] != buildinfo.Version {
		t.Fatalf("incorrect version: %s, code=%d, %s", &out, code, &stderr)
	}
}

func TestInvalidCLIInput(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yml")
	writeFile(t, cfg, "{}\n")
	checks := filepath.Join(dir, "checks.json")
	writeFile(t, checks, "null")
	badGeminiChecks := filepath.Join(dir, "bad-gemini-checks.json")
	writeFile(t, badGeminiChecks, `{"invalid.key":"Question?"}`)
	for _, args := range [][]string{
		{"--example", "--staged", "--dry-run"},
		{"--example", "--checks", checks, "--dry-run"},
		{"--example", "--timeout", "NaN", "--dry-run"},
		{"--example", "--timeout", "0", "--dry-run"},
		{"--example", "--output", "yaml", "--dry-run"},
		{"--example", "--provider", "unknown", "--dry-run"},
		{"--example", "--provider", "", "--dry-run"},
		{"--example", "--model", "", "--dry-run"},
		{"--example", "--endpoint", "ftp://bad.example.com", "--dry-run"},
		{"--example", "--provider", "litellm", "--dry-run"},
		{"--example", "--provider", "litellm", "--model", "jev", "--dry-run"},
		{"--example", "--provider", "gemini", "--checks", badGeminiChecks, "--dry-run"},
		{"--dry-run"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := Run(append([]string{"--config", cfg}, args...), strings.NewReader(""), &out, &stderr)
			if code != 2 || out.Len() != 0 {
				t.Fatalf("code=%d, out=%s, stderr=%s", code, &out, &stderr)
			}
		})
	}
}

func TestStagedDiffInUnbornRepository(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yml")
	writeFile(t, cfg, "{}\n")
	for _, args := range [][]string{{"init", "--quiet", dir}, {"-C", dir, "add", "sample.py"}} {
		if args[0] == "-C" {
			writeFile(t, filepath.Join(dir, "sample.py"), "answer = 42\n")
		}
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	t.Chdir(dir)
	var out, stderr bytes.Buffer
	code := Run([]string{"--config", cfg, "--staged", "--dry-run"}, strings.NewReader(""), &out, &stderr)
	if code != 0 || !strings.Contains(out.String(), "+answer = 42") {
		t.Fatalf("staged diff lost: code=%d, out=%s, stderr=%s", code, &out, &stderr)
	}
}

func TestStagedOutsideGitRepository(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yml")
	writeFile(t, cfg, "{}\n")
	t.Chdir(dir)
	var out, stderr bytes.Buffer
	code := Run([]string{"--config", cfg, "--staged", "--dry-run"}, strings.NewReader(""), &out, &stderr)
	if code != 1 || out.Len() != 0 || !strings.Contains(stderr.String(), "Git repository worktree") || strings.Contains(stderr.String(), "--no-index") {
		t.Fatalf("unexpected non-repo --staged output: code=%d, out=%s, stderr=%s", code, &out, &stderr)
	}
}

func TestDefaultChecksAreIndependent(t *testing.T) {
	checks := DefaultChecks()
	delete(checks, "changes_behavior")
	if len(DefaultChecks()) != 3 {
		t.Fatal("defaults mutated")
	}
	request, err := BuildRequest(ExampleDiff, "", "", nil)
	if err != nil || len(request.Questions) != 3 {
		t.Fatalf("invalid embedded example: %#v, %v", request, err)
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInputSizeLimits(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yml")
	writeFile(t, cfg, "{}\n")
	for _, source := range []string{"stdin", "file", "checks"} {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/%d-extra", source, extra), func(t *testing.T) {
				args := []string{"--config", cfg, "--dry-run"}
				input := ""
				switch source {
				case "stdin":
					input = strings.Repeat("x", maxDiffBytes+extra)
				case "file":
					path := filepath.Join(dir, "change.diff")
					writeFile(t, path, strings.Repeat("x", maxDiffBytes+extra))
					args = append(args, "--diff", path)
				case "checks":
					path := filepath.Join(dir, "checks.json")
					const check = `{"check":"Question?"}`
					writeFile(t, path, check+strings.Repeat(" ", maxChecksBytes-len(check)+extra))
					args = append(args, "--example", "--checks", path)
				}
				var stderr bytes.Buffer
				code := Run(args, strings.NewReader(input), io.Discard, &stderr)
				if extra == 0 && code != 0 {
					t.Fatalf("exact boundary rejected: code=%d, %s", code, &stderr)
				}
				if extra == 1 && (code != 2 || !strings.Contains(stderr.String(), "exceeds")) {
					t.Fatalf("oversized input accepted: code=%d, %s", code, &stderr)
				}
			})
		}
	}
}

func TestBuildRequestSizeLimits(t *testing.T) {
	for _, extra := range []int{0, 1} {
		for _, kind := range []string{"diff", "context"} {
			t.Run(fmt.Sprintf("%s/%d-extra", kind, extra), func(t *testing.T) {
				change, contextText := "+ x", ""
				if kind == "diff" {
					change = strings.Repeat("x", maxDiffBytes+extra)
				} else {
					contextText = strings.Repeat("x", maxContextBytes+extra)
				}
				_, err := BuildRequest(change, contextText, "", nil)
				if extra == 0 && err != nil {
					t.Fatalf("exact boundary rejected: %v", err)
				}
				if extra == 1 && (err == nil || !strings.Contains(err.Error(), kind+" exceeds")) {
					t.Fatalf("oversized %s accepted: %v", kind, err)
				}
			})
		}
	}
}

func TestStagedDiffSizeLimit(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yml")
	writeFile(t, cfg, "{}\n")
	git := func(args ...string) []byte {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return out
	}
	git("init", "--quiet")
	path := filepath.Join(dir, "sample.txt")
	writeFile(t, path, "x\n")
	git("add", "sample.txt")
	// One line of text keeps the diff header length fixed as its content grows.
	overhead := len(git("diff", "--cached", "--no-ext-diff", "--no-textconv")) - 2
	t.Chdir(dir)
	for _, extra := range []int{0, 1} {
		writeFile(t, path, strings.Repeat("x", maxDiffBytes-overhead-1+extra)+"\n")
		git("add", "sample.txt")
		if actual := len(git("diff", "--cached", "--no-ext-diff", "--no-textconv")); actual != maxDiffBytes+extra {
			t.Fatalf("fixture size changed: got %d, want %d", actual, maxDiffBytes+extra)
		}
		var stderr bytes.Buffer
		code := Run([]string{"--config", cfg, "--staged", "--dry-run"}, strings.NewReader(""), io.Discard, &stderr)
		if extra == 0 && code != 0 {
			t.Fatalf("exact boundary rejected: code=%d, %s", code, &stderr)
		}
		if extra == 1 && (code != 2 || !strings.Contains(stderr.String(), "exceeds")) {
			t.Fatalf("oversized staged diff accepted: code=%d, %s", code, &stderr)
		}
	}
}

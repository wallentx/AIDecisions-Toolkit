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
		name    string
		args    []string
		model   string
		context string
	}{
		{"tool", nil, "tool-model", "tool context"},
		{"flags", []string{"--model", "flag-model", "--context", ""}, "flag-model", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"--config", cfg, "--example", "--dry-run"}, test.args...)
			var out, stderr bytes.Buffer
			if code := Run(args, strings.NewReader(""), &out, &stderr); code != 0 {
				t.Fatalf("code %d: %s", code, &stderr)
			}
			var request decisions.Request
			if err := json.Unmarshal(out.Bytes(), &request); err != nil {
				t.Fatal(err)
			}
			var input map[string]string
			if err := json.Unmarshal([]byte(request.Input), &input); err != nil {
				t.Fatal(err)
			}
			if request.Model != test.model || input["context"] != test.context || len(request.Questions) != 1 || request.Questions[0].Name != "custom" {
				t.Fatalf("incorrect effective settings: %#v, %#v", request, input)
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
					answer := decisions.Answer{Name: question.Name, Type: "predicate", Probability: &p}
					if test.refusal && i == 0 {
						answer.Type, answer.Probability = "refusal", nil
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
	for _, args := range [][]string{
		{"--example", "--staged", "--dry-run"},
		{"--example", "--checks", checks, "--dry-run"},
		{"--example", "--timeout", "NaN", "--dry-run"},
		{"--example", "--timeout", "0", "--dry-run"},
		{"--example", "--output", "yaml", "--dry-run"},
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

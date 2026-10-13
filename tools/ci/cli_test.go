package ci

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"aid/internal/github"
)

func TestCLIVersionAndHelpBypassConfiguration(t *testing.T) {
	t.Setenv("AID_CONFIG", filepath.Join(t.TempDir(), "missing.yml"))
	for _, args := range [][]string{{"--version", "--json"}, {"--help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr, func(github.Client, github.Client) API { t.Fatal("API factory called"); return nil }); code != 0 {
			t.Errorf("run(%v) code = %d, want 0; stderr=%q", args, code, stderr.String())
		}
	}
}

func TestCLIDryRunConfigPrecedenceAndInterspersedFlags(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	config := "version: 1\nglobal:\n  timeout_seconds: 30\n  output: json\ntools:\n  ci:\n    timeout_seconds: 99\n    max_log_lines: 42\n"
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatalf("os.WriteFile(config) error: %v", err)
	}
	t.Setenv("AID_CONFIG", configPath)
	var stdout, stderr bytes.Buffer
	args := []string{"https://github.com/octo/example/actions/runs/123", "--dry-run", "--json", "--timeout", "5"}
	code := run(args, &stdout, &stderr, func(github.Client, github.Client) API { t.Fatal("API factory called"); return nil })
	if code != 0 {
		t.Fatalf("run(dry-run) code = %d, want 0; stderr=%q", code, stderr.String())
	}
	var preview struct {
		Timeout float64          `json:"timeout_seconds"`
		Limits  map[string]int64 `json:"limits"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &preview); err != nil {
		t.Fatalf("json.Unmarshal(dry-run) error: %v", err)
	}
	if preview.Timeout != 5 || preview.Limits["max_log_lines"] != 42 {
		t.Errorf("dry-run timeout/max_log_lines = %v/%d, want 5/42", preview.Timeout, preview.Limits["max_log_lines"])
	}
}

func TestCLIInvalidArgumentsExitTwo(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("version: 1\nglobal:\n  timeout_seconds: 30\n  output: json\n"), 0600); err != nil {
		t.Fatalf("os.WriteFile(config) error: %v", err)
	}
	t.Setenv("AID_CONFIG", configPath)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--repo", "bad", "--run", "1"}, &stdout, &stderr, func(github.Client, github.Client) API { return newFakeAPI() }); code != 2 {
		t.Errorf("run(invalid args) code = %d, want 2", code)
	}
}

func TestCLICompleteAndPartialExitCodes(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("version: 1\nglobal:\n  timeout_seconds: 30\n  output: json\n"), 0600); err != nil {
		t.Fatalf("os.WriteFile(config) error: %v", err)
	}
	t.Setenv("AID_CONFIG", configPath)
	for _, test := range []struct {
		name       string
		status     string
		conclusion string
		want       int
	}{{"complete", "completed", "success", 0}, {"ongoing", "in_progress", "", 3}} {
		t.Run(test.name, func(t *testing.T) {
			api := newFakeAPI()
			ref := testRef(1)
			runValue := runJSON(ref, 1, test.conclusion)
			runValue["status"] = test.status
			endpoint := fmt.Sprintf("repos/octo/example/actions/runs/%d/attempts/1", ref.RunID)
			api.queueREST(endpoint, runValue, runValue)
			api.queueREST(endpoint+"/jobs?per_page=100&page=1", map[string]any{"total_count": 0, "jobs": []any{}})
			queueArtifacts(api, ref)
			var stdout, stderr bytes.Buffer
			args := []string{"--repo", "octo/example", "--run", fmt.Sprint(ref.RunID), "--attempt", "1", "--json"}
			code := run(args, &stdout, &stderr, func(github.Client, github.Client) API { return api })
			if code != test.want || !json.Valid(stdout.Bytes()) {
				t.Errorf("run(%s) code/valid = %d/%v, want %d/true; stderr=%q", test.name, code, json.Valid(stdout.Bytes()), test.want, stderr.String())
			}
		})
	}
}

func TestCLIOutputFailureExitOne(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("version: 1\nglobal:\n  timeout_seconds: 30\n  output: json\n"), 0600); err != nil {
		t.Fatalf("os.WriteFile(config) error: %v", err)
	}
	t.Setenv("AID_CONFIG", configPath)
	var stderr bytes.Buffer
	writer := errorWriter{}
	code := run([]string{"--repo", "octo/example", "--run", "123", "--dry-run"}, writer, &stderr, func(github.Client, github.Client) API {
		t.Fatal("API factory called")
		return nil
	})
	if code != 1 {
		t.Errorf("run(output failure) code = %d, want 1", code)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

var _ io.Writer = errorWriter{}

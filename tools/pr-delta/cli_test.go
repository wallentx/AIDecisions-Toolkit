package prdelta

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aid/internal/github"
)

func configForCLI(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	content := "version: 1\nglobal:\n  timeout_seconds: 10\n  output: json\ntools:\n  pr-delta:\n    hostname: github.com\n    max_pages: 5\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func noAPI(t *testing.T) func(github.Client) API {
	return func(github.Client) API {
		t.Error("unexpected API construction")
		return nil
	}
}

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return result
}

func TestCLIVersion(t *testing.T) {
	var out, errs bytes.Buffer
	if code := run([]string{"--version", "--json"}, &out, &errs, noAPI(t)); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, &errs)
	}
	if result := decode(t, out.Bytes()); result["command"] != "AID.pr-delta" || result["tool_version"] != "0.1.0" {
		t.Errorf("version = %v", result)
	}
}

func TestCLIDryRunUsesConfigWithoutAPI(t *testing.T) {
	var out, errs bytes.Buffer
	code := run([]string{"https://github.com/acme/widget/pull/7", "--dry-run", "--config", configForCLI(t), "--since-commit", "ABCDEF1"}, &out, &errs, noAPI(t))
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, &errs)
	}
	result := decode(t, out.Bytes())
	limits := result["limits"].(map[string]any)
	if limits["max_pages"] != 5.0 || result["baseline"] != "--since-commit abcdef1" || result["read_only"] != true {
		t.Errorf("dry-run = %v", result)
	}
}

func TestCLIRejectsInvalidRequestsBeforeAPI(t *testing.T) {
	configPath := configForCLI(t)
	for _, args := range [][]string{
		{"--repo", "acme/widget", "--pr", "0"},
		{"--repo", "acme/widget", "--pr", "7", "--since-commit", "xyz1234"},
		{"--repo", "acme/widget", "--pr", "7", "--since-commit", "abc"},
		{"--repo", "acme/widget", "--pr", "7", "--reviewer", "bad login"},
		{"--repo", "acme/widget", "--pr", "7", "--json", "--text"},
		{"--repo", "acme/widget", "--pr", "7", "--max-output-bytes", "10"},
		{"https://elsewhere.example/acme/widget/pull/7"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, errs bytes.Buffer
			if code := run(append([]string{"--config", configPath}, args...), &out, &errs, noAPI(t)); code != 2 || out.Len() != 0 {
				t.Errorf("exit = %d, stdout = %s, stderr = %s", code, &out, &errs)
			}
		})
	}
}

func TestCLIExitCodes(t *testing.T) {
	configPath := configForCLI(t)
	stable := func(t *testing.T) *fakeAPI {
		fake := newFake(t, pullAt(headSHA, "main", "open"))
		fake.rest[compareEP(mainTip, headSHA)] = compareOf("ahead", mergeBase1, nil, nil)
		return fake
	}
	moved := func(t *testing.T) *fakeAPI {
		fake := stable(t)
		fake.pulls = append(fake.pulls, pullAt(sha("7"), "main", "open"))
		return fake
	}
	missing := func(t *testing.T) *fakeAPI {
		fake := stable(t)
		fake.rest[reviewsEP()] = []any{}
		return fake
	}
	for _, test := range []struct {
		name string
		fake func(*testing.T) *fakeAPI
		args []string
		code int
	}{
		{"complete", stable, []string{"--since-commit", headSHA}, 0},
		{"head moved", moved, []string{"--since-commit", headSHA}, 3},
		{"baseline missing", missing, nil, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := test.fake(t)
			args := append([]string{"--config", configPath, "--repo", "acme/widget", "--pr", "7", "--reviewer", "octo", "--json"}, test.args...)
			var out, errs bytes.Buffer
			code := run(args, &out, &errs, func(github.Client) API { return fake })
			if code != test.code {
				t.Fatalf("exit = %d, want %d; stderr = %s", code, test.code, &errs)
			}
			if result := decode(t, out.Bytes()); result["schema_version"] != 1.0 || result["complete"] != (test.code == 0) {
				t.Errorf("result complete = %v schema = %v", result["complete"], result["schema_version"])
			}
		})
	}
}

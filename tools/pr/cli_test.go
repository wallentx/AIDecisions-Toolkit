package pr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aid/internal/github"
)

func configForCLI(t *testing.T, toolSettings string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	content := "version: 1\nglobal:\n  timeout_seconds: 10\n  output: json\ntools:\n  pr:\n    hostname: github.com\n" + toolSettings
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func noAPI(t *testing.T) func(github.Client) API {
	t.Helper()
	return func(github.Client) API {
		t.Error("unexpected API construction")
		return nil
	}
}

func TestCLIDryRunUsesConfigAndOverridesWithoutAPI(t *testing.T) {
	configPath := configForCLI(t, "    max_output_bytes: 65536\n    page_size: 40\n    timeout_seconds: 50\n")
	var out, errs bytes.Buffer
	code := run([]string{"https://github.com/owner/repo/pull/42", "--dry-run", "--config", configPath, "--page-size", "20"}, &out, &errs, noAPI(t))
	if code != 0 {
		t.Fatalf("dry-run exit = %d, stderr = %s", code, &errs)
	}
	result := readRendered(t, out.Bytes())
	limits := result["limits"].(map[string]any)
	if limits["max_output_bytes"].(json.Number).String() != "65536" || limits["page_size"].(json.Number).String() != "20" || result["timeout_seconds"].(json.Number).String() != "50" {
		t.Errorf("resolved dry-run = %#v, want configured output/timeout and overridden page size", result)
	}
}

func TestCLIVersionDoesNotLoadConfiguration(t *testing.T) {
	t.Setenv("AID_CONFIG", filepath.Join(t.TempDir(), "missing.yml"))
	var out, errs bytes.Buffer
	if code := run([]string{"--version", "--json"}, &out, &errs, noAPI(t)); code != 0 {
		t.Fatalf("version exit = %d, stderr = %s", code, &errs)
	}
	result := readRendered(t, out.Bytes())
	if result["command"] != "AID.pr" || result["tool_version"] != "0.1.0" {
		t.Errorf("version = %#v", result)
	}
}

func TestCLIRejectsInvalidRequestsBeforeAPI(t *testing.T) {
	configPath := configForCLI(t, "")
	for _, args := range [][]string{
		{"--repo", "owner/repo", "--pr", "0"},
		{"https://elsewhere.example/owner/repo/pull/1"},
		{"--repo", "owner/repo", "--pr", "1", "--timeout", "NaN"},
		{"--repo", "owner/repo", "--pr", "1", "--max-output-bytes", "32"},
		{"--repo", "owner/repo", "--pr", "1", "--json", "--text"},
		{"--repo", "owner/repo", "--pr", "1", "--page-size", "101"},
		{"--repo", "owner/repo", "--pr", "1", "--max-requests", "1"},
		{"https://github.com/owner/repo/pull/1", "--gh"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, errs bytes.Buffer
			combined := append([]string{"--config", configPath}, args...)
			if code := run(combined, &out, &errs, noAPI(t)); code != 2 || out.Len() != 0 {
				t.Errorf("invalid request exit = %d, stdout = %s, stderr = %s", code, &out, &errs)
			}
		})
	}
}

type cliAPI struct {
	metadataCalls int
	drift         bool
	failure       bool
}

func (api *cliAPI) REST(_ context.Context, endpoint string, out any) error {
	if api.failure {
		return &github.Error{Kind: github.KindPermissionDenied, Message: "GitHub denied the request"}
	}
	response := "[]"
	switch {
	case strings.HasSuffix(endpoint, "/pulls/42"):
		api.metadataCalls++
		head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if api.drift && api.metadataCalls > 1 {
			head = "cccccccccccccccccccccccccccccccccccccccc"
		}
		response = `{"id":1,"number":42,"html_url":"https://github.com/owner/repo/pull/42","title":"Example","body":"short","state":"open","draft":false,"merged":false,"updated_at":"2026-10-13T00:00:00Z","changed_files":0,"head":{"sha":"` + head + `","ref":"topic","repo":{"id":1,"full_name":"owner/repo"}},"base":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","ref":"main","repo":{"id":1,"full_name":"owner/repo"}}}`
	case strings.Contains(endpoint, "/check-runs"):
		response = `{"total_count":0,"check_runs":[]}`
	}
	return json.Unmarshal([]byte(response), out)
}

func (api *cliAPI) GraphQL(_ context.Context, _ string, _ map[string]any, out any) error {
	return json.Unmarshal([]byte(`{"repository":{"pullRequest":{"reviewThreads":{"totalCount":0,"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`), out)
}

func TestCLICompleteAndChangedEvidenceExitCodes(t *testing.T) {
	configPath := configForCLI(t, "")
	for _, test := range []struct {
		name string
		api  cliAPI
		want int
	}{{name: "complete", want: 0}, {name: "changed", api: cliAPI{drift: true}, want: 3}, {name: "unavailable", api: cliAPI{failure: true}, want: 3}} {
		t.Run(test.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			code := run([]string{"--repo", "owner/repo", "--pr", "42", "--config", configPath}, &out, &errs, func(github.Client) API { return &test.api })
			if code != test.want {
				t.Fatalf("run(%s) exit = %d, want %d; stderr %s; stdout %s", test.name, code, test.want, &errs, &out)
			}
			result := readRendered(t, out.Bytes())
			if result["complete"] != (test.want == 0) {
				t.Errorf("complete = %v, want %v", result["complete"], test.want == 0)
			}
		})
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("closed output") }

func TestCLIOutputFailureIsFatal(t *testing.T) {
	var errs bytes.Buffer
	if code := run([]string{"--version"}, brokenWriter{}, &errs, noAPI(t)); code != 1 {
		t.Errorf("output failure exit = %d, want 1", code)
	}
}

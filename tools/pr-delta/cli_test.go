package prdelta

import (
	"bytes"
	"context"
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

// cliFake is a minimal read-only transport for exit-code tests. The
// comparison itself is covered in internal/prdelta.
type cliFake struct {
	t         *testing.T
	pulls     []map[string]any
	pullReads int
	rest      map[string]any
}

func (fake *cliFake) REST(_ context.Context, endpoint string, out any) error {
	if endpoint == "repos/acme/widget/pulls/7" {
		index := min(fake.pullReads, len(fake.pulls)-1)
		fake.pullReads++
		return copyJSON(out, fake.pulls[index])
	}
	if value, ok := fake.rest[endpoint]; ok {
		return copyJSON(out, value)
	}
	switch {
	case strings.Contains(endpoint, "/check-runs?"):
		return copyJSON(out, map[string]any{"total_count": 0, "check_runs": []any{}})
	case strings.Contains(endpoint, "/status?"):
		return copyJSON(out, map[string]any{"total_count": 0, "statuses": []any{}})
	}
	fake.t.Errorf("unexpected REST endpoint %q", endpoint)
	return &github.Error{Kind: github.KindNotFound, Message: "not found"}
}

func (fake *cliFake) GraphQL(_ context.Context, _ string, _ map[string]any, out any) error {
	threads := map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{}}
	return copyJSON(out, map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewDecision": "APPROVED", "reviewThreads": threads}}})
}

func copyJSON(out, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func cliPull(head string) map[string]any {
	return map[string]any{"number": 7, "state": "open", "mergeable_state": "clean",
		"head": map[string]any{"ref": "topic", "sha": head, "repo": map[string]any{"full_name": "acme/widget"}},
		"base": map[string]any{"ref": "main", "sha": strings.Repeat("e", 40), "repo": map[string]any{"full_name": "acme/widget", "default_branch": "main"}}}
}

func TestCLIExitCodes(t *testing.T) {
	configPath := configForCLI(t)
	headSHA := strings.Repeat("2", 40)
	compareEP := "repos/acme/widget/compare/main..." + headSHA + "?per_page=100&page=1"
	stable := func(t *testing.T) *cliFake {
		return &cliFake{t: t, pulls: []map[string]any{cliPull(headSHA)}, rest: map[string]any{
			compareEP: map[string]any{"status": "ahead", "ahead_by": 0, "total_commits": 0,
				"merge_base_commit": map[string]any{"sha": strings.Repeat("3", 40)}, "commits": []any{}, "files": []any{}},
		}}
	}
	moved := func(t *testing.T) *cliFake {
		fake := stable(t)
		fake.pulls = append(fake.pulls, cliPull(strings.Repeat("7", 40)))
		return fake
	}
	missing := func(t *testing.T) *cliFake {
		fake := stable(t)
		fake.rest["repos/acme/widget/pulls/7/reviews?per_page=100&page=1"] = []any{}
		return fake
	}
	for _, test := range []struct {
		name string
		fake func(*testing.T) *cliFake
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

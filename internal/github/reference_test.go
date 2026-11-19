package github

import (
	"math"
	"reflect"
	"strconv"
	"testing"
)

func TestParsePullRequestForms(t *testing.T) {
	want := PullRequest{Repository: Repository{Host: "ghe.example.com", Owner: "Acme", Name: "widget.go"}, Number: 42}
	for _, test := range []struct {
		name      string
		raw, repo string
		number    int
		host      string
	}{
		{name: "url", raw: "https://GHE.Example.com/Acme/widget.go/pull/42", host: "ghe.example.com"},
		{name: "flags", repo: "Acme/widget.go", number: 42, host: "GHE.Example.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParsePullRequest(test.raw, test.repo, test.number, test.host)
			if err != nil {
				t.Fatalf("ParsePullRequest() error: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ParsePullRequest() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestParsePullRequestDefaultsToGitHub(t *testing.T) {
	got, err := ParsePullRequest("https://github.com/acme/widget/pull/7", "", 0, "")
	if err != nil {
		t.Fatalf("ParsePullRequest() error: %v", err)
	}
	want := PullRequest{Repository: Repository{Host: "github.com", Owner: "acme", Name: "widget"}, Number: 7}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParsePullRequest() = %+v, want %+v", got, want)
	}
}

func TestParsePullRequestRejectsAmbiguousOrUnsafeInput(t *testing.T) {
	for _, test := range []struct {
		name      string
		raw, repo string
		number    int
		host      string
	}{
		{name: "mixed", raw: "https://github.com/a/b/pull/1", repo: "a/b", number: 1},
		{name: "missing", repo: "a/b"},
		{name: "zero", repo: "a/b", number: 0},
		{name: "negative", repo: "a/b", number: -1},
		{name: "repo traversal", repo: "../b", number: 1},
		{name: "owner punctuation prefix", repo: "-a/b", number: 1},
		{name: "repo extra segment", repo: "a/b/c", number: 1},
		{name: "repo control", repo: "a/b\n", number: 1},
		{name: "credentials", raw: "https://user:pass@github.com/a/b/pull/1"},
		{name: "query", raw: "https://github.com/a/b/pull/1?x=1"},
		{name: "fragment", raw: "https://github.com/a/b/pull/1#x"},
		{name: "encoded path", raw: "https://github.com/a/%62/pull/1"},
		{name: "encoded traversal", raw: "https://github.com/a/%2e%2e/pull/1"},
		{name: "host mismatch", raw: "https://evil.example/a/b/pull/1"},
		{name: "port override", raw: "https://github.com:443/a/b/pull/1"},
		{name: "http", raw: "http://github.com/a/b/pull/1"},
		{name: "trailing slash", raw: "https://github.com/a/b/pull/1/"},
		{name: "non-number", raw: "https://github.com/a/b/pull/main"},
		{name: "zero url", raw: "https://github.com/a/b/pull/0"},
		{name: "invalid host", repo: "a/b", number: 1, host: "https://github.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParsePullRequest(test.raw, test.repo, test.number, test.host); err == nil {
				t.Errorf("ParsePullRequest(%q, %q, %d, %q) accepted unsafe input", test.raw, test.repo, test.number, test.host)
			}
		})
	}
}

func TestParsePullRequestAcceptsRepositoryPunctuationPrefixes(t *testing.T) {
	for _, name := range []string{".github", "_project", "-project"} {
		got, err := ParsePullRequest("", "owner/"+name, 42, "")
		if err != nil || got.Name != name {
			t.Errorf("ParsePullRequest(repository %q) = %+v, %v", name, got, err)
		}
	}
}

func TestParseWorkflowRunForms(t *testing.T) {
	tests := []struct {
		name                 string
		raw, repo, host      string
		runID, wantRunID     int64
		jobID                int64
		attempt, wantAttempt int
	}{
		{name: "run URL", raw: "https://GHE.Example.com/Acme/widget.go/actions/runs/42", host: "ghe.example.com", wantRunID: 42},
		{name: "attempt URL", raw: "https://ghe.example.com/Acme/widget.go/actions/runs/42/attempts/3", host: "GHE.Example.com", wantRunID: 42, wantAttempt: 3},
		{name: "largest native attempt", raw: "https://ghe.example.com/Acme/widget.go/actions/runs/42/attempts/" + strconv.Itoa(math.MaxInt), host: "ghe.example.com", wantRunID: 42, wantAttempt: math.MaxInt},
		{name: "64-bit job ID", raw: "https://ghe.example.com/Acme/widget.go/actions/runs/42/job/9223372036854775807", host: "ghe.example.com", wantRunID: 42, jobID: math.MaxInt64},
		{name: "job URL", raw: "https://ghe.example.com/Acme/widget.go/actions/runs/42/job/99", host: "ghe.example.com", wantRunID: 42, jobID: 99},
		{name: "run URL with attempt", raw: "https://ghe.example.com/Acme/widget.go/actions/runs/42", host: "ghe.example.com", wantRunID: 42, attempt: 2, wantAttempt: 2},
		{name: "attempt URL with matching attempt", raw: "https://ghe.example.com/Acme/widget.go/actions/runs/42/attempts/3", host: "ghe.example.com", wantRunID: 42, attempt: 3, wantAttempt: 3},
		{name: "flags latest", repo: "Acme/widget.go", host: "GHE.Example.com", runID: 42, wantRunID: 42},
		{name: "flags pinned", repo: "Acme/widget.go", host: "ghe.example.com", runID: 42, wantRunID: 42, attempt: 4, wantAttempt: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseWorkflowRun(test.raw, test.repo, test.runID, test.attempt, test.host)
			if err != nil {
				t.Fatalf("ParseWorkflowRun() error: %v", err)
			}
			want := WorkflowRun{
				Repository: Repository{Host: "ghe.example.com", Owner: "Acme", Name: "widget.go"},
				RunID:      test.wantRunID,
				Attempt:    test.wantAttempt,
				JobID:      test.jobID,
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ParseWorkflowRun() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestParseWorkflowRunDefaultsToGitHub(t *testing.T) {
	got, err := ParseWorkflowRun("https://github.com/acme/widget/actions/runs/7", "", 0, 0, "")
	if err != nil {
		t.Fatalf("ParseWorkflowRun() error: %v", err)
	}
	want := WorkflowRun{
		Repository: Repository{Host: "github.com", Owner: "acme", Name: "widget"},
		RunID:      7,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseWorkflowRun() = %+v, want %+v", got, want)
	}
}

func TestParseWorkflowRunRejectsAmbiguousOrUnsafeInput(t *testing.T) {
	tests := []struct {
		name            string
		raw, repo, host string
		runID           int64
		attempt         int
	}{
		{name: "mixed repository", raw: "https://github.com/a/b/actions/runs/1", repo: "a/b"},
		{name: "mixed run ID", raw: "https://github.com/a/b/actions/runs/1", runID: 1},
		{name: "missing", repo: "a/b"},
		{name: "zero run ID", repo: "a/b", runID: 0},
		{name: "negative run ID", repo: "a/b", runID: -1},
		{name: "negative attempt", repo: "a/b", runID: 1, attempt: -1},
		{name: "attempt conflict", raw: "https://github.com/a/b/actions/runs/1/attempts/2", attempt: 3},
		{name: "repo traversal", repo: "../b", runID: 1},
		{name: "repo extra segment", repo: "a/b/c", runID: 1},
		{name: "credentials", raw: "https://user:pass@github.com/a/b/actions/runs/1"},
		{name: "query", raw: "https://github.com/a/b/actions/runs/1?x=1"},
		{name: "fragment", raw: "https://github.com/a/b/actions/runs/1#x"},
		{name: "encoded path", raw: "https://github.com/a/%62/actions/runs/1"},
		{name: "encoded traversal", raw: "https://github.com/a/%2e%2e/actions/runs/1"},
		{name: "control", raw: "https://github.com/a/b/actions/runs/1\n"},
		{name: "host mismatch", raw: "https://evil.example/a/b/actions/runs/1"},
		{name: "port override", raw: "https://github.com:443/a/b/actions/runs/1"},
		{name: "http", raw: "http://github.com/a/b/actions/runs/1"},
		{name: "trailing slash", raw: "https://github.com/a/b/actions/runs/1/"},
		{name: "non-number run", raw: "https://github.com/a/b/actions/runs/main"},
		{name: "zero URL run", raw: "https://github.com/a/b/actions/runs/0"},
		{name: "non-number attempt", raw: "https://github.com/a/b/actions/runs/1/attempts/main"},
		{name: "zero URL attempt", raw: "https://github.com/a/b/actions/runs/1/attempts/0"},
		{name: "signed attempt", raw: "https://github.com/a/b/actions/runs/1/attempts/+1"},
		{name: "attempt exceeds native int", raw: "https://github.com/a/b/actions/runs/1/attempts/" + strconv.FormatUint(uint64(math.MaxInt)+1, 10)},
		{name: "non-number job", raw: "https://github.com/a/b/actions/runs/1/job/main"},
		{name: "zero job", raw: "https://github.com/a/b/actions/runs/1/job/0"},
		{name: "unknown suffix", raw: "https://github.com/a/b/actions/runs/1/jobs/2"},
		{name: "extra suffix", raw: "https://github.com/a/b/actions/runs/1/attempts/2/job/3"},
		{name: "invalid host", repo: "a/b", runID: 1, host: "https://github.com"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseWorkflowRun(test.raw, test.repo, test.runID, test.attempt, test.host); err == nil {
				t.Errorf(
					"ParseWorkflowRun(%q, %q, %d, %d, %q) accepted unsafe input",
					test.raw,
					test.repo,
					test.runID,
					test.attempt,
					test.host,
				)
			}
		})
	}
}

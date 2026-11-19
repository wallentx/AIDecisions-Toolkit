package github

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// Repository identifies a GitHub repository on a configured host.
type Repository struct {
	Host  string `json:"host"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// PullRequest identifies a pull request and its repository.
type PullRequest struct {
	Repository
	Number int `json:"number"`
}

// WorkflowRun identifies a workflow run, optional attempt, and optional job.
// Attempt zero means the latest attempt must be resolved and pinned by the
// caller before collecting mutable run data.
type WorkflowRun struct {
	Repository
	RunID   int64 `json:"run_id"`
	Attempt int   `json:"attempt"`
	JobID   int64 `json:"job_id,omitempty"`
}

// ParseWorkflowRun accepts either a workflow-run URL or an owner/name, positive
// run ID, and optional positive attempt. A URL may be combined only with an
// attempt that agrees with the URL or supplies an attempt absent from the URL.
func ParseWorkflowRun(raw string, repo string, runID int64, attempt int, host string) (WorkflowRun, error) {
	selectedHost, err := normalizeHost(host)
	if err != nil {
		return WorkflowRun{}, err
	}
	if attempt < 0 {
		return WorkflowRun{}, errors.New("workflow run attempt cannot be negative")
	}
	if raw != "" {
		if repo != "" || runID != 0 {
			return WorkflowRun{}, errors.New("workflow run URL cannot be combined with repository or run ID")
		}
		ref, err := parseWorkflowRunURL(raw, selectedHost)
		if err != nil {
			return WorkflowRun{}, err
		}
		if attempt != 0 {
			if ref.Attempt != 0 && ref.Attempt != attempt {
				return WorkflowRun{}, errors.New("workflow run attempt does not match URL")
			}
			ref.Attempt = attempt
		}
		return ref, nil
	}
	if repo == "" || runID <= 0 {
		return WorkflowRun{}, errors.New("repository and positive workflow run ID are required")
	}
	owner, name, ok := parseRepository(repo)
	if !ok {
		return WorkflowRun{}, errors.New("invalid GitHub repository")
	}
	return WorkflowRun{
		Repository: Repository{Host: selectedHost, Owner: owner, Name: name},
		RunID:      runID,
		Attempt:    attempt,
	}, nil
}

func parseWorkflowRunURL(raw string, selectedHost string) (WorkflowRun, error) {
	if raw != strings.TrimSpace(raw) || strings.Contains(raw, "%") {
		return WorkflowRun{}, errors.New("invalid GitHub workflow run URL")
	}
	for _, char := range raw {
		if unicode.IsControl(char) {
			return WorkflowRun{}, errors.New("invalid GitHub workflow run URL")
		}
	}
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return WorkflowRun{}, errors.New("invalid GitHub workflow run URL")
	}
	if parsed.Host == "" || parsed.Host != parsed.Hostname() {
		return WorkflowRun{}, errors.New("invalid GitHub workflow run URL")
	}
	urlHost, err := normalizeHost(parsed.Hostname())
	if err != nil || urlHost != selectedHost {
		return WorkflowRun{}, errors.New("GitHub workflow run host does not match selected host")
	}

	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 5 && len(parts) != 7 {
		return WorkflowRun{}, errors.New("invalid GitHub workflow run URL")
	}
	owner, name, ok := parseRepository(parts[0] + "/" + parts[1])
	if !ok || parts[2] != "actions" || parts[3] != "runs" {
		return WorkflowRun{}, errors.New("invalid GitHub workflow run URL")
	}
	runID, ok := parsePositiveInt64(parts[4])
	if !ok {
		return WorkflowRun{}, errors.New("invalid GitHub workflow run URL")
	}
	ref := WorkflowRun{
		Repository: Repository{Host: selectedHost, Owner: owner, Name: name},
		RunID:      runID,
	}
	if len(parts) == 5 {
		return ref, nil
	}
	switch parts[5] {
	case "attempts":
		attempt, err := strconv.Atoi(parts[6])
		if err != nil || attempt <= 0 || !allDigits(parts[6]) {
			return WorkflowRun{}, errors.New("invalid GitHub workflow run URL")
		}
		ref.Attempt = attempt
	case "job":
		jobID, ok := parsePositiveInt64(parts[6])
		if !ok {
			return WorkflowRun{}, errors.New("invalid GitHub workflow run URL")
		}
		ref.JobID = jobID
	default:
		return WorkflowRun{}, errors.New("invalid GitHub workflow run URL")
	}
	return ref, nil
}

func parsePositiveInt64(value string) (int64, bool) {
	if !allDigits(value) {
		return 0, false
	}
	number, err := strconv.ParseInt(value, 10, 64)
	return number, err == nil && number > 0
}

// ParsePullRequest accepts either a pull-request URL or an owner/name and
// positive number. The two forms cannot be mixed.
func ParsePullRequest(raw string, repo string, number int, host string) (PullRequest, error) {
	selectedHost, err := normalizeHost(host)
	if err != nil {
		return PullRequest{}, err
	}
	if raw != "" {
		if repo != "" || number != 0 {
			return PullRequest{}, errors.New("pull request forms cannot be mixed")
		}
		return parsePullRequestURL(raw, selectedHost)
	}
	if repo == "" || number <= 0 {
		return PullRequest{}, errors.New("repository and positive pull request number are required")
	}
	owner, name, ok := parseRepository(repo)
	if !ok {
		return PullRequest{}, errors.New("invalid GitHub repository")
	}
	return PullRequest{Repository: Repository{Host: selectedHost, Owner: owner, Name: name}, Number: number}, nil
}

func parsePullRequestURL(raw string, selectedHost string) (PullRequest, error) {
	if raw != strings.TrimSpace(raw) || strings.Contains(raw, "%") {
		return PullRequest{}, errors.New("invalid GitHub pull request URL")
	}
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return PullRequest{}, errors.New("invalid GitHub pull request URL")
	}
	if parsed.Host == "" || parsed.Host != parsed.Hostname() {
		return PullRequest{}, errors.New("invalid GitHub pull request URL")
	}
	urlHost, err := normalizeHost(parsed.Hostname())
	if err != nil || urlHost != selectedHost {
		return PullRequest{}, errors.New("GitHub pull request host does not match selected host")
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return PullRequest{}, errors.New("invalid GitHub pull request URL")
	}
	owner, name, ok := parseRepository(parts[0] + "/" + parts[1])
	if !ok || !allDigits(parts[3]) {
		return PullRequest{}, errors.New("invalid GitHub pull request URL")
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number <= 0 {
		return PullRequest{}, errors.New("invalid GitHub pull request URL")
	}
	return PullRequest{Repository: Repository{Host: selectedHost, Owner: owner, Name: name}, Number: number}, nil
}

func parseRepository(repo string) (owner string, name string, ok bool) {
	if repo != strings.TrimSpace(repo) {
		return "", "", false
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || !validRepositoryPart(parts[0]) || !validRepositoryPart(parts[1]) {
		return "", "", false
	}
	if char := parts[0][0]; (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func validRepositoryPart(part string) bool {
	if part == "" || part == "." || part == ".." || len(part) > 100 {
		return false
	}
	for _, char := range part {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' && char != '.' {
			return false
		}
	}
	return true
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type release struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

type githubClient struct {
	client  *http.Client
	baseURL string
}

func (g githubClient) latest(ctx context.Context, repository string) (release, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+"/repos/"+repository+"/releases/latest", nil)
	if err != nil {
		return release{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "AID-updater")
	response, err := g.client.Do(request)
	if err != nil {
		return release{}, fmt.Errorf("check GitHub release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return release{}, fmt.Errorf("no published release found for %s (repository must be public)", repository)
	}
	if response.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("GitHub release check returned HTTP %d", response.StatusCode)
	}
	var result release
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return release{}, fmt.Errorf("decode GitHub release: %w", err)
	}
	if result.Draft || result.Prerelease {
		return release{}, fmt.Errorf("GitHub latest release %q is not a stable published release", result.TagName)
	}
	if _, err := canonicalVersion(result.TagName); err != nil {
		return release{}, err
	}
	return result, nil
}

type commandRunner func(context.Context, string, io.Writer, string, ...string) error

func runCommand(ctx context.Context, directory string, output io.Writer, command string, args ...string) error {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = directory
	cmd.Stdout = output
	cmd.Stderr = output
	// Updates use a public HTTPS repository; never stop waiting for credentials.
	cmd.Env = gitEnvironment(os.Environ())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", command, strings.Join(args, " "), err)
	}
	return nil
}

func gitEnvironment(environment []string) []string {
	var cleaned []string
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			cleaned = append(cleaned, entry)
		}
	}
	// Inherited GIT_DIR/worktree settings or init templates must not redirect an
	// isolated release checkout into the user's current repository.
	return append(cleaned, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}

func checkoutRelease(ctx context.Context, repository, tag, source string, output io.Writer, run commandRunner) error {
	if !repositoryPattern.MatchString(repository) {
		return fmt.Errorf("invalid repository %q; use owner/repo", repository)
	}
	if _, err := canonicalVersion(tag); err != nil {
		return err
	}
	if err := os.Mkdir(source, 0700); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--template="},
		{"-c", "credential.helper=", "fetch", "--quiet", "--depth=1", "--no-tags", "https://github.com/" + repository + ".git", "refs/tags/" + tag},
		{"-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", "FETCH_HEAD"},
	} {
		if err := run(ctx, source, output, "git", args...); err != nil {
			return err
		}
	}
	return nil
}

func defaultGitHub() githubClient {
	return githubClient{client: &http.Client{Timeout: 30 * time.Second}, baseURL: "https://api.github.com"}
}

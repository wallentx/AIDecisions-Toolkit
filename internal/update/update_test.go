package update

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"aid/internal/catalog"
)

func TestSemanticVersions(t *testing.T) {
	for _, tc := range []struct {
		current, latest string
		newer           bool
	}{
		{"0.2.0", "v0.10.0", true},
		{"1.0.0-rc.9", "v1.0.0-rc.10", true},
		{"1.0.0-rc.10", "v1.0.0", true},
		{"1.0.0", "v1.0.0+rebuild", false},
		{"2.0.0", "v1.9.0", false},
		{"1.0.0", "v1.0.0-rc.1", false},
	} {
		t.Run(tc.current+"/"+tc.latest, func(t *testing.T) {
			got, err := isNewer(tc.current, tc.latest)
			if err != nil || got != tc.newer {
				t.Fatalf("isNewer = %v, %v; want %v", got, err, tc.newer)
			}
		})
	}
	for _, version := range []string{"1", "v1.2", "v01.2.3", "1.2.3-01", "--upload-pack=x", "v1.2.3/evil"} {
		if _, err := canonicalVersion(version); err == nil {
			t.Errorf("accepted invalid version %q", version)
		}
	}
}

func TestGitHubRelease(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantErr    bool
	}{
		{"stable", `{"tag_name":"v0.3.0"}`, 200, false},
		{"draft", `{"tag_name":"v0.3.0","draft":true}`, 200, true},
		{"prerelease", `{"tag_name":"v0.3.0-rc.1","prerelease":true}`, 200, true},
		{"missing", `{}`, 404, true},
		{"rate limited", `{}`, 403, true},
		{"invalid tag", `{"tag_name":"latest"}`, 200, true},
		{"malformed", `nope`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/example/AID/releases/latest" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := githubClient{client: server.Client(), baseURL: server.URL}
			_, err := client.latest(context.Background(), "example/AID")
			if (err != nil) != tc.wantErr {
				t.Fatalf("latest error = %v", err)
			}
		})
	}
}

func TestCheckoutUsesOnlyExactReleaseTag(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	var calls [][]string
	run := func(_ context.Context, dir string, _ io.Writer, command string, args ...string) error {
		if dir != source || command != "git" {
			t.Fatalf("unexpected command location %s: %s", dir, command)
		}
		calls = append(calls, args)
		return nil
	}
	if err := checkoutRelease(context.Background(), "example/AID", "v0.3.0", source, io.Discard, run); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || calls[1][len(calls[1])-1] != "refs/tags/v0.3.0" || calls[2][len(calls[2])-1] != "FETCH_HEAD" {
		t.Fatalf("unexpected checkout commands: %v", calls)
	}
	if err := checkoutRelease(context.Background(), "../other", "v0.3.0", source, io.Discard, run); err == nil {
		t.Fatal("accepted invalid repository")
	}
}

func TestGitCheckoutDoesNotInheritRepositoryOverrides(t *testing.T) {
	environment := gitEnvironment([]string{"PATH=/bin", "GIT_DIR=/my/repo/.git", "git_work_tree=/my/repo", "GIT_CONFIG_COUNT=1", "GIT_TEMPLATE_DIR=/hooks"})
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "/my/repo") || strings.Contains(joined, "/hooks") || strings.Contains(joined, "GIT_CONFIG_COUNT") {
		t.Fatalf("unsafe git environment: %v", environment)
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "GIT_TERMINAL_PROMPT=0") {
		t.Fatalf("missing environment: %v", environment)
	}
}

func TestCheckAndDryRunNeverFetchBuildOrInstall(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		deps := dependencies{
			latest: func(context.Context, string) (release, error) { return release{TagName: "v0.3.0"}, nil },
		}
		var output bytes.Buffer
		err := perform(context.Background(), options{repository: "example/AID", current: "0.2.0", checkOnly: !dryRun, dryRun: dryRun}, deps, &output, io.Discard)
		if err != nil || !strings.Contains(output.String(), "Update available") {
			t.Fatalf("check result: %v %s", err, output.String())
		}
		if dryRun && !strings.Contains(output.String(), "Would build") {
			t.Fatal("dry-run omitted build plan")
		}
	}
}

func fixtureDependencies(t *testing.T) dependencies {
	t.Helper()
	return dependencies{
		goos:   "linux",
		latest: func(context.Context, string) (release, error) { return release{TagName: "v0.3.0"}, nil },
		checkout: func(_ context.Context, _, _, source string, _ io.Writer) error {
			return os.Mkdir(source, 0700)
		},
		tools: func(string) ([]catalog.Tool, error) {
			return []catalog.Tool{{Command: "AID.diff"}, {Command: "AID.new-tool"}}, nil
		},
		build: func(_ context.Context, _, out, tag string, _ io.Writer) error {
			if tag != "v0.3.0" {
				t.Fatalf("expected release tag v0.3.0 passed to builder, got %q", tag)
			}
			if err := os.Mkdir(out, 0700); err != nil {
				return err
			}
			for _, command := range []string{"AID", "AID.diff", "AID.new-tool"} {
				if err := os.WriteFile(filepath.Join(out, command), []byte("new "+command), 0700); err != nil {
					return err
				}
			}
			return nil
		},
		replace: func(dir string, artifacts []artifact) error { return replaceArtifacts(dir, artifacts, os.Rename) },
	}
}

func TestUpdateDiscoversNewReleaseTools(t *testing.T) {
	deps := fixtureDependencies(t)
	dir := t.TempDir()
	var output bytes.Buffer
	err := perform(context.Background(), options{repository: "example/AID", current: "0.2.0", installDir: dir}, deps, &output, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"AID", "AID.diff", "AID.new-tool"} {
		data, err := os.ReadFile(filepath.Join(dir, command))
		if err != nil || string(data) != "new "+command {
			t.Fatalf("%s not installed: %q %v", command, data, err)
		}
	}
	if !strings.Contains(output.String(), "2 tool(s)") {
		t.Fatal(output.String())
	}
}

func TestUpdateRemovesOnlyRetiredRegisteredTools(t *testing.T) {
	deps := fixtureDependencies(t)
	deps.installed = func() ([]catalog.Tool, error) {
		return []catalog.Tool{{Command: "AID.diff"}, {Command: "AID.retired"}}, nil
	}
	dir := t.TempDir()
	for _, name := range []string{"AID.retired", "AID.local"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old "+name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	err := perform(context.Background(), options{repository: "example/AID", current: "0.2.0", installDir: dir}, deps, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "AID.retired")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("obsolete registered tool remains")
	}
	if _, err := os.Stat(filepath.Join(dir, "AID.local")); err != nil {
		t.Fatalf("unknown local tool was removed: %v", err)
	}
}

func TestRollbackRestoresRetiredTools(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AID.retired"), []byte("old retired"), 0755); err != nil {
		t.Fatal(err)
	}
	artifacts := append(writeArtifacts(t, "AID"), artifact{Name: "AID.retired", Remove: true}, artifact{Name: "AID.missing", Remove: true})
	err := replaceArtifacts(dir, artifacts, func(source, target string) error {
		if filepath.Base(target) == "AID" {
			return errors.New("launcher failed")
		}
		return os.Rename(source, target)
	})
	if err == nil || !strings.Contains(err.Error(), "previous installation restored") {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "AID.retired"))
	if err != nil || string(data) != "old retired" {
		t.Fatalf("retired tool was not restored: %q %v", data, err)
	}
}

func TestBuildFailureLeavesInstallationUntouched(t *testing.T) {
	deps := fixtureDependencies(t)
	deps.build = func(context.Context, string, string, string, io.Writer) error { return errors.New("compiler failed") }
	deps.replace = func(string, []artifact) error { t.Fatal("replacement after build failure"); return nil }
	dir := t.TempDir()
	err := perform(context.Background(), options{repository: "example/AID", current: "0.2.0", installDir: dir}, deps, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "compiler failed") {
		t.Fatalf("wrong failure: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("failed build wrote installation directory")
	}
}

func TestUpdateStampsPublishedReleaseTagWithoutVersionFile(t *testing.T) {
	deps := fixtureDependencies(t)
	deps.latest = func(context.Context, string) (release, error) { return release{TagName: "v0.4.0"}, nil }
	var stampedTag string
	deps.build = func(_ context.Context, _, out, tag string, _ io.Writer) error {
		stampedTag = tag
		if err := os.Mkdir(out, 0700); err != nil {
			return err
		}
		for _, command := range []string{"AID", "AID.diff", "AID.new-tool"} {
			if err := os.WriteFile(filepath.Join(out, command), []byte("new "+command), 0700); err != nil {
				return err
			}
		}
		return nil
	}
	dir := t.TempDir()
	if err := perform(context.Background(), options{repository: "example/AID", current: "0.2.0", installDir: dir}, deps, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if stampedTag != "v0.4.0" {
		t.Fatalf("stampedTag = %q, want v0.4.0", stampedTag)
	}
}

func writeArtifacts(t *testing.T, names ...string) []artifact {
	t.Helper()
	dir := t.TempDir()
	var artifacts []artifact
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("new "+name), 0700); err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, artifact{Name: name, Path: path})
	}
	return artifacts
}

func TestReplacementOrderAndRollback(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"AID", "AID.diff"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old "+name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	artifacts := writeArtifacts(t, "AID", "AID.new-tool", "AID.diff")
	var installed []string
	rename := func(source, target string) error {
		if strings.HasPrefix(filepath.Base(source), "new-") {
			installed = append(installed, filepath.Base(target))
			if filepath.Base(target) == "AID" {
				return errors.New("injected launcher install failure")
			}
		}
		return os.Rename(source, target)
	}
	err := replaceArtifacts(dir, artifacts, rename)
	if err == nil || !strings.Contains(err.Error(), "previous installation restored") {
		t.Fatalf("wrong failure: %v", err)
	}
	if !reflect.DeepEqual(installed, []string{"AID.diff", "AID.new-tool", "AID"}) {
		t.Fatalf("wrong install order: %v", installed)
	}
	for _, name := range []string{"AID", "AID.diff"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != "old "+name {
			t.Fatalf("%s was not restored: %q %v", name, data, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "AID.new-tool")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new tool survived rollback")
	}
}

func TestPrepareFailureAndSymlinksDoNotReplaceFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unprivileged Windows symlinks may be unavailable")
	}
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "AID.diff")); err != nil {
		t.Fatal(err)
	}
	err := replaceArtifacts(dir, writeArtifacts(t, "AID", "AID.diff"), os.Rename)
	if err == nil || !strings.Contains(err.Error(), "non-regular") {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "untouched" {
		t.Fatal("symlink target modified")
	}
}

func TestReplacementWaitsForParent(t *testing.T) {
	dir := t.TempDir()
	plan := replacementPlan{ParentPID: 42, InstallDir: dir, Artifacts: writeArtifacts(t, "AID", "AID.diff")}
	waited := false
	err := applyPlan(plan, func(pid int) error { waited = pid == 42; return nil }, func(source, target string) error {
		if !waited {
			t.Fatal("replacement before parent exited")
		}
		return os.Rename(source, target)
	})
	if err != nil || !waited {
		t.Fatalf("helper result: %v", err)
	}
}

func TestFailedRollbackRetainsRecoveryFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AID.diff"), []byte("old diff"), 0755); err != nil {
		t.Fatal(err)
	}
	artifacts := writeArtifacts(t, "AID", "AID.diff")
	rename := func(source, target string) error {
		if filepath.Base(target) == "AID" || strings.HasPrefix(filepath.Base(source), "old-") {
			return errors.New("injected failure")
		}
		return os.Rename(source, target)
	}
	err := replaceArtifacts(dir, artifacts, rename)
	if err == nil || !strings.Contains(err.Error(), "recovery files retained") {
		t.Fatalf("wrong failure: %v", err)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, ".aid-update-*", "old-AID.diff"))
	if len(backups) != 1 {
		t.Fatalf("missing backup: %v", backups)
	}
	data, err := os.ReadFile(backups[0])
	if err != nil || string(data) != "old diff" {
		t.Fatalf("backup corrupted: %s %v", data, err)
	}
}

func TestConcurrentUpdateLock(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, ".aid-update.lock")
	if err := os.WriteFile(lock, []byte("held"), 0600); err != nil {
		t.Fatal(err)
	}
	err := replaceArtifacts(dir, writeArtifacts(t, "AID", "AID.diff"), os.Rename)
	if err == nil || !strings.Contains(err.Error(), "another update may be running") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "AID")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrote while another update held lock")
	}
}

func TestMissingRepositoryIsActionable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("version: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := Run([]string{"--config", path, "--check"}, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "updates.repository") {
		t.Fatalf("missing config result: %d %s", code, stderr.String())
	}
}

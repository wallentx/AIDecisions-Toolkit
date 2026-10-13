package discover

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"aid/internal/buildinfo"
)

func syntheticProject(t *testing.T) string {
	t.Helper()
	project, err := filepath.Abs("/synthetic/repo")
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func syntheticSource(t *testing.T, message string) string {
	t.Helper()
	rows := []map[string]any{{"kind": "user", "message": message, "project": syntheticProject(t), "session_id": "synthetic-session", "timestamp": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), "record": 1, "provider": "codex"}}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func syntheticOptions(t *testing.T) SourceOptions {
	t.Helper()
	return SourceOptions{Input: "-", CacheDir: filepath.Join(canonicalTemp(t), "cache"), Project: syntheticProject(t), Provider: "codex", Days: 30, CacheTTL: 10 * time.Minute}
}
func TestSourceCacheContainsOnlyPreparedEvidence(t *testing.T) {
	options := syntheticOptions(t)
	raw := syntheticSource(t, "Please review repeated deployment failures with api_key=ghp_abcdefghijklmnopqrstuvwx contact alice@example.test")
	bundle, first, err := Load(context.Background(), options, strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if first.Hit || first.ID == "" || first.IDStability != "persistent" || len(bundle.Records) != 1 {
		t.Fatalf("unexpected first load: %+v, records=%d", first, len(bundle.Records))
	}
	_, second, err := Load(context.Background(), options, strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Hit || first.ID != second.ID {
		t.Fatalf("expected same cache snapshot: %+v", second)
	}
	loaded, err := ReadSnapshot(options.CacheDir, first.ID)
	if err != nil || !reflect.DeepEqual(bundle, loaded) {
		t.Fatalf("snapshot lookup mismatch: %v", err)
	}
	entries, err := os.ReadDir(options.CacheDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		path := filepath.Join(options.CacheDir, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("insecure mode: %v", info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"ghp_abcdefghijklmnopqrstuvwx", "alice@example.test", "/synthetic/repo", "synthetic-session"} {
			if strings.Contains(string(data), secret) {
				t.Fatalf("raw private source leaked: %s", entry.Name())
			}
		}
	}
}
func TestSourceCacheInvalidatesContentOptionsAndVersion(t *testing.T) {
	options := syntheticOptions(t)
	raw := syntheticSource(t, "Review recurring deployment failures and validate release workflow behavior.")
	_, first, err := Load(context.Background(), options, strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	variants := []SourceOptions{options, options, options}
	variants[0].Days = 12
	variants[1].Provider = "all"
	variants[2].AllProjects = true
	for _, variant := range variants {
		_, info, err := Load(context.Background(), variant, strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if info.Hit {
			t.Fatalf("options failed to invalidate: %+v", variant)
		}
	}
	changed := strings.Replace(raw, "deployment", "scheduling", 1)
	_, info, err := Load(context.Background(), options, strings.NewReader(changed))
	if err != nil || info.Hit {
		t.Fatalf("content failed to invalidate: %+v %v", info, err)
	}
	oldVersion := buildinfo.Version
	buildinfo.Version = oldVersion + "-synthetic"
	t.Cleanup(func() { buildinfo.Version = oldVersion })
	_, info, err = Load(context.Background(), options, strings.NewReader(raw))
	if err != nil || info.Hit {
		t.Fatalf("version failed to invalidate: %+v %v", info, err)
	}
	if first.ID == "" {
		t.Fatal("missing initial snapshot ID")
	}
}
func TestSourceNoCacheAndMalformedInput(t *testing.T) {
	options := syntheticOptions(t)
	options.NoCache = true
	raw := syntheticSource(t, "Verify recurring deployment failures and document the successful workflow.")
	_, info, err := Load(context.Background(), options, strings.NewReader(raw))
	if err != nil || info.IDStability != "ephemeral" {
		t.Fatalf("no-cache: %+v %v", info, err)
	}
	if _, err = os.Stat(options.CacheDir); !os.IsNotExist(err) {
		t.Fatalf("no-cache wrote a directory: %v", err)
	}
	malformed := raw[:len(raw)-1] + `,{"message":"secret=never-disclose"}`
	_, _, err = Load(context.Background(), options, strings.NewReader(malformed))
	if err == nil || strings.Contains(err.Error(), "never-disclose") {
		t.Fatalf("unsafe malformed error: %v", err)
	}
	options.MaxInputBytes = 8
	if _, _, err = Load(context.Background(), options, strings.NewReader(raw)); err == nil {
		t.Fatal("size limit ignored")
	}
}
func fakeReflect(t *testing.T, body string) string {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("shell unavailable for synthetic helper")
	}
	path := filepath.Join(t.TempDir(), "fake-reflect")
	if err = os.WriteFile(path, []byte("#!"+shell+"\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestReflectFixedArgvCacheTTLRefreshAndFailures(t *testing.T) {
	options := syntheticOptions(t)
	options.Input = ""
	options.AllProjects = true
	options.Project = "/synthetic/project;touch SHOULD_NOT_EXIST"
	argvFile := filepath.Join(t.TempDir(), "argv")
	t.Setenv("DISCOVER_FAKE_ARGV", argvFile)
	options.Reflect = fakeReflect(t, `printf '%s\n' "$@" > "$DISCOVER_FAKE_ARGV"
printf '[]'`)
	_, first, err := Load(context.Background(), options, nil)
	if err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"--provider", "codex", "scan", "--project", options.Project, "--days", "30", "--all-projects", "--include-tool-errors", "--format", "json"}
	if !reflect.DeepEqual(strings.Split(strings.TrimSpace(string(argv)), "\n"), expected) {
		t.Fatalf("unexpected helper argv: %q", argv)
	}
	if err = os.Remove(argvFile); err != nil {
		t.Fatal(err)
	}
	_, hit, err := Load(context.Background(), options, nil)
	if err != nil || !hit.Hit || hit.ID != first.ID {
		t.Fatalf("TTL cache failed: %+v %v", hit, err)
	}
	if _, err = os.Stat(argvFile); !os.IsNotExist(err) {
		t.Fatal("cache hit invoked helper")
	}
	options.Refresh = true
	if _, info, err := Load(context.Background(), options, nil); err != nil || info.Hit {
		t.Fatalf("refresh: %+v %v", info, err)
	}
	options.Refresh = false
	options.CacheTTL = time.Nanosecond
	if _, info, err := Load(context.Background(), options, nil); err != nil || info.Hit {
		t.Fatalf("expired TTL: %+v %v", info, err)
	}
	options.Reflect = fakeReflect(t, `printf 'secret=never-disclose' >&2
printf 'secret=never-disclose'
exit 3`)
	if _, _, err = Load(context.Background(), options, nil); err == nil || strings.Contains(err.Error(), "never-disclose") {
		t.Fatalf("unsafe helper failure: %v", err)
	}
	if _, err = ReadSnapshot(options.CacheDir, first.ID); err != nil {
		t.Fatalf("failure destroyed old snapshot: %v", err)
	}
}
func TestExplicitPythonScriptAndTimeout(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python unavailable for synthetic helper")
	}
	_ = python
	options := syntheticOptions(t)
	options.Input = ""
	options.NoCache = true
	options.Reflect = filepath.Join(t.TempDir(), "fake.py")
	if err = os.WriteFile(options.Reflect, []byte("print('[]')\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = Load(context.Background(), options, nil); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(options.Reflect, []byte("import time\ntime.sleep(10)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	options.Timeout = 40 * time.Millisecond
	started := time.Now()
	if _, _, err = Load(context.Background(), options, nil); err == nil {
		t.Fatal("timeout ignored")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("timeout took too long")
	}
}

func canonicalTemp(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFileCacheUsesContentIdentity(t *testing.T) {
	options := syntheticOptions(t)
	options.Input = filepath.Join(canonicalTemp(t), "synthetic.json")
	raw := syntheticSource(t, "Review repeated deployment failures and validate release behavior carefully.")
	if err := os.WriteFile(options.Input, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	_, first, err := Load(context.Background(), options, nil)
	if err != nil || first.Source != "file" {
		t.Fatalf("file load: %+v %v", first, err)
	}
	_, hit, err := Load(context.Background(), options, nil)
	if err != nil || !hit.Hit {
		t.Fatalf("file cache: %+v %v", hit, err)
	}
	if err := os.WriteFile(options.Input, []byte(strings.Replace(raw, "deployment", "scheduling", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	_, changed, err := Load(context.Background(), options, nil)
	if err != nil || changed.Hit || changed.SourceID == first.SourceID {
		t.Fatalf("file content invalidation: %+v %v", changed, err)
	}
}

func TestZeroTTLAndBlockingInputCancellation(t *testing.T) {
	options := syntheticOptions(t)
	options.CacheTTL = 0
	raw := syntheticSource(t, "Review orchard irrigation reports and summarize recurring calibration failures.")
	if _, _, err := Load(context.Background(), options, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if _, info, err := Load(context.Background(), options, strings.NewReader(raw)); err != nil || info.Hit {
		t.Fatalf("zero TTL must expire: %+v %v", info, err)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	options.NoCache = true
	options.Timeout = 25 * time.Millisecond
	before := time.Now()
	if _, _, err := Load(context.Background(), options, reader); err == nil {
		t.Fatal("blocked stdin ignored cancellation")
	}
	if time.Since(before) > time.Second {
		t.Fatal("canceled stdin remained blocked")
	}
	if _, err := writer.Write([]byte("[]")); err == nil {
		t.Fatal("canceled pipe reader was not closed")
	}
}

func TestEmptyInputRejectsSensitiveProviderBeforeMetadataAndCache(t *testing.T) {
	for _, provider := range []string{"alice@example.test", "/private/provider", "two words", "ghp_abcdefghijklmnopqrstuvwx", "sk-abcdefghijklmnopqrstuvwx", "--all-projects"} {
		t.Run(provider, func(t *testing.T) {
			options := syntheticOptions(t)
			options.Provider = provider
			bundle, info, err := Load(context.Background(), options, strings.NewReader("[]"))
			if err == nil || bundle != nil {
				t.Fatal("unsafe provider accepted for empty input")
			}
			encoded, _ := json.Marshal(info)
			if strings.Contains(string(encoded), provider) || strings.Contains(err.Error(), provider) {
				t.Fatal("unsafe provider leaked into metadata or error")
			}
			if _, err := os.Stat(options.CacheDir); !os.IsNotExist(err) {
				t.Fatal("unsafe provider touched cache")
			}
		})
	}
	options := syntheticOptions(t)
	options.Provider = "all"
	options.NoCache = true
	if _, _, err := Load(context.Background(), options, strings.NewReader("[]")); err != nil {
		t.Fatalf("all provider must be allowed: %v", err)
	}
}

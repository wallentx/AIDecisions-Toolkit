package discover

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type cliFixture struct{ root, config, project, cache string }

func isolatedCLI(t *testing.T) cliFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "COPILOT_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	f := cliFixture{root: root, config: filepath.Join(root, "config.yml"), project: filepath.Join(root, "project"), cache: filepath.Join(root, "cache")}
	if err := os.Mkdir(f.project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, []byte("version: 1\ntools:\n  discover: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f cliFixture) run(t *testing.T, input string, args ...string) (int, []byte, string) {
	t.Helper()
	var out, errout bytes.Buffer
	base := []string{"--config", f.config, "--project", f.project, "--cache-dir", f.cache}
	code := Run(append(base, args...), strings.NewReader(input), &out, &errout)
	return code, out.Bytes(), errout.String()
}

func cliRows(t *testing.T, count int, message string) string {
	t.Helper()
	rows := make([]map[string]any, count)
	for i := range rows {
		rows[i] = map[string]any{"provider": "codex", "project": "/synthetic/repo", "session_id": fmt.Sprintf("session-%d", i), "timestamp": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), "kind": "user", "message": message, "record": i}
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func decodeCLIReport(t *testing.T, code int, data []byte, stderr string) report {
	t.Helper()
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	var got report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(data) > got.BudgetBytes {
		t.Fatalf("output=%d exceeds budget=%d", len(data), got.BudgetBytes)
	}
	return got
}

func TestCLIInputCacheSnapshotAndEvidence(t *testing.T) {
	f := isolatedCLI(t)
	raw := cliRows(t, 3, strings.Repeat("Inspect orchard irrigation calibration and rainfall measurements. ", 50))
	code, data, stderr := f.run(t, raw, "--input", "-", "--all-projects")
	first := decodeCLIReport(t, code, data, stderr)
	if first.Mode != "leads" || first.Cache.Hit || first.Cache.ID == "" || len(first.Leads) == 0 || first.Coverage.RetainedRecords != 3 {
		t.Fatalf("unexpected initial report: %+v", first)
	}
	code, data, stderr = f.run(t, raw, "--input", "-", "--all-projects")
	second := decodeCLIReport(t, code, data, stderr)
	if !second.Cache.Hit || second.Cache.ID != first.Cache.ID {
		t.Fatalf("expected cache hit: %+v", second.Cache)
	}
	code, data, stderr = f.run(t, "", "--snapshot", first.Cache.ID, "--evidence", first.Leads[0].ID, "--page-size", "1")
	group := decodeCLIReport(t, code, data, stderr)
	if group.Mode != "evidence" || len(group.Evidence) != 1 || group.Page.NextOffset == nil || *group.Page.NextOffset != 1 {
		t.Fatalf("unexpected group page: %+v", group)
	}
	id := group.Evidence[0].ID
	code, data, stderr = f.run(t, "", "--snapshot", first.Cache.ID, "--evidence", id, "--text-limit", "23")
	part := decodeCLIReport(t, code, data, stderr)
	if len(part.Evidence) != 1 || len([]rune(part.Evidence[0].Text)) != 23 || part.Evidence[0].NextTextOffset == nil {
		t.Fatalf("unexpected text page: %+v", part)
	}
	code, data, stderr = f.run(t, "", "--snapshot", first.Cache.ID, "--evidence", id, "--text-offset", "23", "--text-limit", "23")
	next := decodeCLIReport(t, code, data, stderr)
	if next.Evidence[0].TextOffset != 23 || next.Evidence[0].ID != id {
		t.Fatal("record continuation changed identity or offset")
	}
	code, data, stderr = f.run(t, "", "--snapshot", first.Cache.ID, "--records", "--page-size", "2")
	records := decodeCLIReport(t, code, data, stderr)
	if records.Mode != "records" || len(records.Evidence) != 2 || records.Page.Total != 3 {
		t.Fatalf("unexpected records: %+v", records.Page)
	}
}

func directoryState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			state[path] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		state[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestCLIDryRunAndNoCacheDoNotWrite(t *testing.T) {
	f := isolatedCLI(t)
	before := directoryState(t, f.root)
	code, data, stderr := f.run(t, "not JSON", "--input", filepath.Join(f.root, "does-not-exist"), "--dry-run")
	if code != 0 || !json.Valid(data) || !bytes.Contains(data, []byte(`"writes": false`)) {
		t.Fatalf("dry run failed: %d %s %s", code, data, stderr)
	}
	if after := directoryState(t, f.root); !reflect.DeepEqual(before, after) {
		t.Fatal("dry run wrote files")
	}
	raw := cliRows(t, 2, "Inspect orchard irrigation calibration and rainfall measurements.")
	var ids []string
	for range 2 {
		code, data, stderr = f.run(t, raw, "--input", "-", "--all-projects", "--no-cache", "--records", "--page-size", "1")
		got := decodeCLIReport(t, code, data, stderr)
		if got.Cache.IDStability != "ephemeral" || len(got.Evidence) != 1 || len(got.Page.NextArgs) != 0 {
			t.Fatalf("unexpected no-cache report: %+v", got)
		}
		ids = append(ids, got.Evidence[0].ID)
	}
	if ids[0] == ids[1] {
		t.Fatal("no-cache IDs must be ephemeral")
	}
	if after := directoryState(t, f.root); !reflect.DeepEqual(before, after) {
		t.Fatal("no-cache wrote files")
	}
}

func TestCLIIndependentToolConfig(t *testing.T) {
	f := isolatedCLI(t)
	config := "version: 1\ntools:\n  diff:\n    page_size: invalid-for-discover\n    provider: irrelevant\n  discover:\n    page_size: 1\n    min_sessions: 1\n"
	if err := os.WriteFile(f.config, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	raw := cliRows(t, 3, "Inspect orchard irrigation calibration and rainfall measurements.")
	code, data, stderr := f.run(t, raw, "--input", "-", "--all-projects", "--no-cache", "--records")
	got := decodeCLIReport(t, code, data, stderr)
	if got.Page.Returned != 1 {
		t.Fatalf("discover page size ignored: %+v", got.Page)
	}
	code, data, stderr = f.run(t, raw, "--input", "-", "--all-projects", "--no-cache", "--records", "--page-size", "2")
	got = decodeCLIReport(t, code, data, stderr)
	if got.Page.Returned != 2 {
		t.Fatal("CLI did not override discover config")
	}
}

func TestCLIVersionUsesManifest(t *testing.T) {
	f := isolatedCLI(t)
	code, data, stderr := f.run(t, "", "--version", "--json")
	if code != 0 {
		t.Fatalf("version: %s", stderr)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(ManifestJSON, &manifest); err != nil {
		t.Fatal(err)
	}
	if got["command"] != manifest["command"] || got["tool_version"] != manifest["version"] {
		t.Fatalf("version diverged from manifest: %v", got)
	}
}

func TestCLIRejectsInvalidInputWithoutEcho(t *testing.T) {
	f := isolatedCLI(t)
	secret := "private-malformed-payload-938472"
	code, data, stderr := f.run(t, `[{"message":"`+secret+`",broken]`, "--input", "-", "--no-cache")
	if code == 0 || bytes.Contains(data, []byte(secret)) || strings.Contains(stderr, secret) {
		t.Fatalf("malformed input was accepted or echoed: exit=%d", code)
	}
	for _, args := range [][]string{{"--snapshot", "../../escape"}, {"--offset", "-1"}, {"--page-size", "0"}, {"--max-output-bytes", "4095"}, {"--text-limit", "0"}, {"--days", "0"}, {"--timeout", "NaN"}, {"--evidence", "x", "--records"}} {
		code, _, _ := f.run(t, "", args...)
		if code == 0 {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
}

func TestCLIExplicitInputHonorsTimeScope(t *testing.T) {
	f := isolatedCLI(t)
	var rows []map[string]any
	if err := json.Unmarshal([]byte(cliRows(t, 2, "Inspect orchard irrigation calibration and rainfall measurements.")), &rows); err != nil {
		t.Fatal(err)
	}
	rows[1]["timestamp"] = time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339Nano)
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	code, data, stderr := f.run(t, string(raw), "--input", "-", "--all-projects", "--days", "1", "--no-cache", "--records")
	got := decodeCLIReport(t, code, data, stderr)
	if got.Coverage.InputRecords != 2 || got.Coverage.RetainedRecords != 1 || len(got.Evidence) != 1 {
		t.Fatalf("time scope not applied: %+v", got.Coverage)
	}
}

func TestCLIContinuationFragmentsPreserveExplicitContext(t *testing.T) {
	f := isolatedCLI(t)
	raw := cliRows(t, 3, strings.Repeat("Inspect orchard irrigation calibration and rainfall measurements. ", 40))
	code, data, stderr := f.run(t, raw, "--input", "-", "--all-projects", "--records", "--page-size", "1", "--text-limit", "30")
	first := decodeCLIReport(t, code, data, stderr)
	for _, path := range []string{f.root, f.project, f.config, f.cache} {
		if bytes.Contains(data, []byte(path)) {
			t.Fatal("private invocation path leaked into continuation output")
		}
	}
	if len(first.RequiredContextFlags) == 0 {
		t.Fatal("missing explicit continuation context requirements")
	}
	for _, args := range [][]string{first.Page.NextArgs, first.Evidence[0].NextArgs} {
		if len(args) == 0 {
			t.Fatal("missing persistent continuation arguments")
		}
		var out, errout bytes.Buffer
		base := []string{"--config", f.config, "--project", f.project, "--cache-dir", f.cache}
		code := Run(append(base, args...), strings.NewReader(""), &out, &errout)
		got := decodeCLIReport(t, code, out.Bytes(), errout.String())
		if got.Cache.ID != first.Cache.ID || !got.Cache.Hit {
			t.Fatal("continuation did not use original snapshot")
		}
	}
}

func TestCLISnapshotUsesEvidenceProviderInventory(t *testing.T) {
	f := isolatedCLI(t)
	skill := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "skills", "synthetic")
	if err := os.MkdirAll(skill, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: synthetic\ndescription: Orchard calibration metadata.\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	raw := strings.ReplaceAll(cliRows(t, 2, "Inspect orchard irrigation calibration and rainfall measurements."), `"provider":"codex"`, `"provider":"claude"`)
	code, data, stderr := f.run(t, raw, "--input", "-", "--all-projects", "--provider", "claude")
	first := decodeCLIReport(t, code, data, stderr)
	if first.Inventory == nil || first.Inventory.SkillsSeen != 1 {
		t.Fatal("synthetic provider inventory not loaded")
	}
	code, data, stderr = f.run(t, "", "--snapshot", first.Cache.ID)
	got := decodeCLIReport(t, code, data, stderr)
	if got.Inventory == nil || got.Inventory.SkillsSeen != 1 {
		t.Fatalf("snapshot lost provider inventory: %+v", got.Inventory)
	}
}

//go:build linux || android

package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIDryRunConfigPrecedenceAndNoInspection(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	config := "version: 1\nglobal:\n  timeout_seconds: 11\n  output: json\ntools:\n  runtime:\n    timeout_seconds: 22\n    max_file_bytes: 99\n"
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "host.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1,"observe_host":true,"paths":["/missing"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"/does/not/exist", "--config", configPath, "--host-manifest", manifestPath, "--dry-run", "--timeout", "33"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var preview map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview["timeout_seconds"] != float64(33) {
		t.Fatalf("precedence mismatch: %s", stdout.String())
	}
}

func TestCLIVersionBypassesInvalidConfigAndSelectionErrors(t *testing.T) {
	badConfig := writeFile(t, "bad.yml", []byte("not: valid: yaml"))
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--version", "--config", badConfig}, nil, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), `"tool_version":"0.1.0"`) {
		t.Fatalf("version code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, args := range [][]string{{}, {"a", "b"}, {"a", "--file", "b"}, {"a", "--json", "--text"}} {
		stdout.Reset()
		stderr.Reset()
		if code := Run(args, nil, &stdout, &stderr); code != 2 {
			t.Fatalf("args=%v code=%d", args, code)
		}
	}
}

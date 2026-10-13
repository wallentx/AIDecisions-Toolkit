//go:build linux || android

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aid/internal/bounded"
)

func TestHostManifestStrictValidation(t *testing.T) {
	valid := `{"schema_version":1,"observe_host":true,"paths":["/a","/b"]}`
	for _, test := range []struct {
		name, data string
		ok         bool
	}{
		{"valid", valid, true},
		{"unknown", `{"schema_version":1,"observe_host":true,"paths":[],"extra":1}`, false},
		{"duplicate_key", `{"schema_version":1,"schema_version":1,"observe_host":true,"paths":[]}`, false},
		{"duplicate_path", `{"schema_version":1,"observe_host":true,"paths":["/a","/a"]}`, false},
		{"relative", `{"schema_version":1,"observe_host":true,"paths":["a"]}`, false},
		{"unclean", `{"schema_version":1,"observe_host":true,"paths":["/a/../b"]}`, false},
		{"paths_without_opt_in", `{"schema_version":1,"observe_host":false,"paths":["/a"]}`, false},
		{"wrong_version", `{"schema_version":2,"observe_host":true,"paths":[]}`, false},
		{"wrong_type", `{"schema_version":1,"observe_host":"yes","paths":[]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeFile(t, "manifest.json", []byte(test.data))
			manifest, err := LoadHostManifest(path)
			if test.ok != (err == nil) || (test.ok && len(manifest.Paths) != 2) {
				t.Fatalf("manifest=%+v err=%v", manifest, err)
			}
		})
	}
}

func TestManifestSizeAndEnvelopeBounds(t *testing.T) {
	path := writeFile(t, "large.json", []byte(strings.Repeat(" ", maximumManifestBytes+1)))
	if _, err := LoadHostManifest(path); !errors.Is(err, bounded.ErrLimit) {
		t.Fatalf("oversized manifest error=%v", err)
	}
	paths := make([]string, 16)
	for index := range paths {
		paths[index] = "/" + strings.Repeat(string(rune('a'+index)), 240)
	}
	manifest := &HostManifest{SchemaVersion: 1, ObserveHost: true, Paths: paths}
	if err := PreflightEnvelope("/target", manifest, Options{}); err == nil || !strings.Contains(err.Error(), "4096-byte") {
		t.Fatalf("expected fixed envelope rejection, got %v", err)
	}
}

func TestPathObservationStates(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present")
	symlink := filepath.Join(dir, "link")
	dangling := filepath.Join(dir, "dangling")
	if err := os.WriteFile(present, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(present, symlink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "absent-target"), dangling); err != nil {
		t.Fatal(err)
	}
	report := baseReport("/target")
	observeHost(&report, &HostManifest{SchemaVersion: 1, ObserveHost: true, Paths: []string{present, symlink, filepath.Join(dir, "missing"), dangling}})
	if report.Host.Paths[0].State != "present" || report.Host.Paths[0].Kind != "regular" || report.Host.Paths[1].Kind != "symlink" || report.Host.Paths[2].State != "missing" || report.Host.Paths[3].State != "missing" || report.Host.Paths[3].Kind != "symlink" {
		t.Fatalf("unexpected observations: %+v", report.Host.Paths)
	}
	if state := lookupPathState(report.Host.Paths, "/not-listed"); state != "unchecked" {
		t.Fatalf("unlisted state=%s", state)
	}
}

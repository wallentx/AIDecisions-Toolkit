package runtime

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRuntimeAcceptanceManifestObservationsAndUncheckedInterpreter(t *testing.T) {
	directory := t.TempDir()
	present := filepath.Join(directory, "present")
	missing := filepath.Join(directory, "missing")
	dangling := filepath.Join(directory, "dangling")
	if err := os.WriteFile(present, []byte("present"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(missing, dangling); err != nil {
		t.Fatal(err)
	}
	target := runtimeAcceptanceELF(t, directory, dangling)
	manifest := &HostManifest{SchemaVersion: 1, ObserveHost: true, Paths: []string{present, missing, dangling}}

	report, err := Collect(context.Background(), target, manifest, Options{})
	if err != nil {
		t.Fatalf("Collect(%q, manifest) error: %v", target, err)
	}
	if !report.Complete || report.ELF == nil {
		t.Fatalf("Collect(%q, manifest) = %+v, want complete ELF report", target, report)
	}
	wantPaths := []PathObservation{
		{Path: present, State: "present", Kind: "regular"},
		{Path: missing, State: "missing"},
		{Path: dangling, State: "missing", Kind: "symlink"},
	}
	if !slices.Equal(report.Host.Paths, wantPaths) {
		t.Errorf("Collect(%q, manifest) host paths = %#v, want %#v", target, report.Host.Paths, wantPaths)
	}
	if got, want := report.ELF.InterpreterObservation, "missing"; got != want {
		t.Errorf("Collect(%q, manifest) interpreter observation = %q, want %q", target, got, want)
	}

	withoutManifest, err := Collect(context.Background(), target, nil, Options{})
	if err != nil {
		t.Fatalf("Collect(%q, nil) error: %v", target, err)
	}
	if got, want := withoutManifest.Host.Observation, "unchecked"; got != want {
		t.Errorf("Collect(%q, nil) host observation = %q, want %q", target, got, want)
	}
	if withoutManifest.ELF == nil || withoutManifest.ELF.InterpreterObservation != "unchecked" {
		t.Errorf("Collect(%q, nil) interpreter facts = %#v, want unchecked", target, withoutManifest.ELF)
	}
}

func TestRuntimeAcceptanceNonELFIsStructuredUnsupported(t *testing.T) {
	path := runtimeAcceptanceWrite(t, "plain-text", []byte(strings.Repeat("not an executable\n", 8)))
	report, err := Collect(context.Background(), path, nil, Options{})
	if err != nil {
		t.Fatalf("Collect(%q) error: %v", path, err)
	}
	if report.Complete || report.Status != "unsupported" || report.Issue == nil || report.Issue.Code != "not_elf" {
		t.Errorf("Collect(%q) = %+v, want incomplete unsupported not_elf report", path, report)
	}
}

func TestRuntimeAcceptanceMalformedTablesRemainIncomplete(t *testing.T) {
	directory := t.TempDir()
	for _, test := range []struct {
		name string
		edit func([]byte)
		code string
	}{
		{
			name: "section_table_outside_file",
			edit: func(data []byte) {
				binary.LittleEndian.PutUint64(data[40:48], uint64(len(data)+1))
				binary.LittleEndian.PutUint16(data[58:60], 64)
				binary.LittleEndian.PutUint16(data[60:62], 1)
			},
			code: "section_header_bounds",
		},
		{
			name: "extended_program_header_count",
			edit: func(data []byte) { binary.LittleEndian.PutUint16(data[56:58], 0xffff) },
			code: "extended_header_numbering",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := runtimeAcceptanceELF(t, directory, "")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			test.edit(data)
			if err := os.WriteFile(path, data, 0700); err != nil {
				t.Fatal(err)
			}
			report, err := Collect(context.Background(), path, nil, Options{})
			if err != nil {
				t.Fatalf("Collect(%q) error: %v", path, err)
			}
			if report.Complete || report.Issue == nil || report.Issue.Code != test.code {
				t.Errorf("Collect(%q) = %+v, want incomplete report with issue %q", path, report, test.code)
			}
		})
	}
}

func TestRuntimeAcceptanceManifestFIFOIsRejectedWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.fifo")
	if err := syscallMkfifo(path); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := LoadHostManifest(path)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Errorf("LoadHostManifest(%q) error = %v, want regular-file rejection", path, err)
		}
	case <-time.After(time.Second):
		t.Fatalf("LoadHostManifest(%q) blocked on FIFO", path)
	}
}

func TestRuntimeAcceptanceRawFileAttributionIsBoundedAndDeterministic(t *testing.T) {
	path := runtimeAcceptanceELF(t, t.TempDir(), "")
	report, err := Collect(context.Background(), path, nil, Options{})
	if err != nil {
		t.Fatalf("Collect(%q) error: %v", path, err)
	}
	if !report.Complete || report.Target.ResolvedPath != path || report.Target.SizeBytes <= 0 || report.Target.SHA256 == "" || !report.Provenance.ReadOnly || report.Provenance.Atomic {
		t.Errorf("Collect(%q) attribution = %+v, want complete read-only raw-file attribution", path, report)
	}
	wantUnknowns := []string{"dependency_resolution", "child_launch_environment", "execution_policy", "actual_launch_success"}
	if !slices.Equal(report.Unknowns, wantUnknowns) {
		t.Errorf("Collect(%q) unknowns = %#v, want %#v", path, report.Unknowns, wantUnknowns)
	}
	first, firstComplete, err := Render(report)
	if err != nil {
		t.Fatalf("Render(first report) error: %v", err)
	}
	second, secondComplete, err := Render(report)
	if err != nil {
		t.Fatalf("Render(second report) error: %v", err)
	}
	if firstComplete != secondComplete || !firstComplete || len(first) > MaximumOutputBytes || !json.Valid(first) || !slices.Equal(first, second) {
		t.Errorf("Render(report) deterministic packet: first=%d bytes complete=%v second=%d bytes complete=%v, want equal valid complete packets <= %d bytes", len(first), firstComplete, len(second), secondComplete, MaximumOutputBytes)
	}
}

func runtimeAcceptanceELF(t *testing.T, directory, interpreter string) string {
	t.Helper()
	programHeaders := uint16(0)
	interpreterOffset := 64
	if interpreter != "" {
		programHeaders = 1
		interpreterOffset = 120
	}
	data := make([]byte, interpreterOffset+len(interpreter)+1)
	copy(data[:4], []byte{0x7f, 'E', 'L', 'F'})
	data[4], data[5], data[6] = 2, 1, 1             // ELF64, little-endian, current version.
	binary.LittleEndian.PutUint16(data[16:18], 3)   // ET_DYN
	binary.LittleEndian.PutUint16(data[18:20], 183) // EM_AARCH64
	binary.LittleEndian.PutUint32(data[20:24], 1)
	binary.LittleEndian.PutUint16(data[52:54], 64)
	if programHeaders != 0 {
		binary.LittleEndian.PutUint64(data[32:40], 64)
		binary.LittleEndian.PutUint16(data[54:56], 56)
		binary.LittleEndian.PutUint16(data[56:58], programHeaders)
		program := data[64:120]
		binary.LittleEndian.PutUint32(program[:4], 3) // PT_INTERP
		binary.LittleEndian.PutUint64(program[8:16], uint64(interpreterOffset))
		binary.LittleEndian.PutUint64(program[32:40], uint64(len(interpreter)+1))
		binary.LittleEndian.PutUint64(program[40:48], uint64(len(interpreter)+1))
		copy(data[interpreterOffset:], interpreter)
	}
	return runtimeAcceptanceWriteAt(t, filepath.Join(directory, "synthetic-aarch64.elf"), data)
}

func runtimeAcceptanceWrite(t *testing.T, name string, data []byte) string {
	t.Helper()
	return runtimeAcceptanceWriteAt(t, filepath.Join(t.TempDir(), name), data)
}

func runtimeAcceptanceWriteAt(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

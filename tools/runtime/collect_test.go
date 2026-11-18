//go:build linux || android

package runtime

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func writeELF(t *testing.T, name string, class byte, machine, elfType uint16, interpreter string) string {
	t.Helper()
	phnum := uint16(0)
	dataOffset := uint64(64)
	if interpreter != "" {
		phnum, dataOffset = 1, 120
	}
	size := int(dataOffset)
	if interpreter != "" {
		size += len(interpreter) + 1
	}
	data := make([]byte, size)
	copy(data[:4], []byte{0x7f, 'E', 'L', 'F'})
	data[4], data[5], data[6] = class, byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(data[16:18], elfType)
	binary.LittleEndian.PutUint16(data[18:20], machine)
	binary.LittleEndian.PutUint32(data[20:24], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(data[52:54], 64)
	if phnum > 0 {
		binary.LittleEndian.PutUint64(data[32:40], 64)
		binary.LittleEndian.PutUint16(data[54:56], 56)
		binary.LittleEndian.PutUint16(data[56:58], phnum)
		ph := data[64:120]
		binary.LittleEndian.PutUint32(ph[:4], uint32(elf.PT_INTERP))
		binary.LittleEndian.PutUint64(ph[8:16], dataOffset)
		binary.LittleEndian.PutUint64(ph[32:40], uint64(len(interpreter)+1))
		binary.LittleEndian.PutUint64(ph[40:48], uint64(len(interpreter)+1))
		copy(data[dataOffset:], interpreter)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCollectSyntheticGlibcAbsentAndUnchecked(t *testing.T) {
	interpreter := "/lib/ld-linux-aarch64.so.1"
	target := writeELF(t, "dynamic-glibc", 2, uint16(elf.EM_AARCH64), uint16(elf.ET_DYN), interpreter)
	manifest := &HostManifest{SchemaVersion: 1, ObserveHost: true, Paths: []string{interpreter}}
	report, err := Collect(context.Background(), target, manifest, Options{})
	if err != nil || !report.Complete || report.ELF == nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if report.ELF.Interpreter != interpreter || report.ELF.InterpreterObservation != "missing" || report.ELF.Type != "shared_object" {
		t.Fatalf("unexpected ELF facts: %+v", report.ELF)
	}
	if report.Provenance.Parser.Name != "go/debug/elf" || report.Provenance.Parser.ToolVersion != "0.1.1" {
		t.Fatalf("unexpected parser provenance: %+v", report.Provenance.Parser)
	}
	withoutManifest, err := Collect(context.Background(), target, nil, Options{})
	if err != nil || withoutManifest.ELF.InterpreterObservation != "unchecked" || withoutManifest.Host.Observation != "unchecked" {
		t.Fatalf("unchecked facts: %+v %v", withoutManifest, err)
	}
}

func TestCollectNativeBionicArtifact(t *testing.T) {
	if goruntime.GOARCH != "arm64" {
		t.Skip("native fixture requires an AArch64 host; synthetic ELF cases cover other architectures")
	}
	target, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifest := &HostManifest{SchemaVersion: 1, ObserveHost: true, Paths: []string{"/system/bin/linker64"}}
	report, err := Collect(context.Background(), target, manifest, Options{})
	if err != nil || !report.Complete || report.ELF == nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if report.ELF.Machine != "AArch64" {
		t.Fatalf("native facts: %+v", report.ELF)
	}
	if goruntime.GOOS == "android" && (report.ELF.Interpreter != "/system/bin/linker64" || report.ELF.InterpreterObservation != "present") {
		t.Fatalf("Android native facts: %+v", report.ELF)
	}
	encoded, complete, err := Render(report)
	if err != nil || !complete || len(encoded) > MaximumOutputBytes || !json.Valid(encoded) {
		t.Fatalf("render complete=%v bytes=%d err=%v", complete, len(encoded), err)
	}
}

func TestUnsupportedAndMalformedTargets(t *testing.T) {
	cases := []struct{ name, path, code string }{
		{"non_elf", writeFile(t, "text", []byte("plain text")), "not_elf"},
		{"truncated_elf", writeFile(t, "short", []byte{0x7f, 'E', 'L', 'F', 2, 1, 1}), "truncated_header"},
		{"wrong_class", writeELF(t, "class", 1, uint16(elf.EM_AARCH64), uint16(elf.ET_DYN), ""), "unsupported_class"},
		{"wrong_machine", writeELF(t, "machine", 2, uint16(elf.EM_X86_64), uint16(elf.ET_DYN), ""), "unsupported_machine"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			report, err := Collect(context.Background(), test.path, nil, Options{})
			if err != nil || report.Issue == nil || report.Issue.Code != test.code {
				t.Fatalf("report=%+v err=%v", report, err)
			}
		})
	}
	badTable := writeELF(t, "bad-table", 2, uint16(elf.EM_AARCH64), uint16(elf.ET_DYN), "")
	data, _ := os.ReadFile(badTable)
	binary.LittleEndian.PutUint64(data[40:48], uint64(len(data)+1))
	binary.LittleEndian.PutUint16(data[58:60], 64)
	binary.LittleEndian.PutUint16(data[60:62], 1)
	os.WriteFile(badTable, data, 0700)
	report, err := Collect(context.Background(), badTable, nil, Options{})
	if err != nil || report.Issue == nil || report.Issue.Code != "section_header_bounds" {
		t.Fatalf("bad table report=%+v err=%v", report, err)
	}
}

func TestOversizeTimeoutMutationAndReplacement(t *testing.T) {
	target := writeELF(t, "payload", 2, uint16(elf.EM_AARCH64), uint16(elf.ET_DYN), "")
	report, err := Collect(context.Background(), target, nil, Options{MaxFileBytes: 16})
	if err != nil || report.Issue == nil || report.Issue.Code != "target_unavailable" {
		t.Fatalf("oversized report=%+v err=%v", report, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	report, err = Collect(cancelled, target, nil, Options{})
	if err != nil || report.Issue == nil || report.Issue.Code != "timeout" || report.Status == "changed" {
		t.Fatalf("timeout report=%+v err=%v", report, err)
	}
	for _, test := range []struct {
		name string
		hook func(string) func()
	}{
		{"mutation", func(path string) func() {
			return func() {
				file, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				_, _ = file.Write([]byte("x"))
				_ = file.Close()
			}
		}},
		{"replacement", func(path string) func() {
			return func() {
				data, _ := os.ReadFile(path)
				replacement := path + ".new"
				_ = os.WriteFile(replacement, data, 0700)
				_ = os.Rename(replacement, path)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			caseTarget := writeELF(t, "payload", 2, uint16(elf.EM_AARCH64), uint16(elf.ET_DYN), "")
			report, err := collect(context.Background(), caseTarget, nil, Options{}, test.hook(caseTarget))
			if err != nil || report.Issue == nil || report.Issue.Code != "input_changed" {
				t.Fatalf("report=%+v err=%v", report, err)
			}
		})
	}
}

func TestNoProductionSubprocessImports(t *testing.T) {
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"os/exec"`) || strings.Contains(string(data), "CommandContext(") {
			t.Fatalf("production runtime contains subprocess code in %s", path)
		}
	}
}

func TestManifestFIFORejectedWithoutRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.fifo")
	if err := syscallMkfifo(path); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if _, err := LoadHostManifest(path); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("FIFO error=%v", err)
	}
}

func TestHashUsesAcceptedInitialSize(t *testing.T) {
	path := writeFile(t, "data", []byte("abc"))
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	first, err := hashFile(context.Background(), file, 3)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("abcdef"), 0600)
	second, err := hashFile(context.Background(), file, 3)
	if err != nil || first != second {
		t.Fatalf("bounded hash changed: %s %s %v", first, second, err)
	}
}

func TestDynamicMetadataRejectsDecoySectionAndBoundsLongStrings(t *testing.T) {
	makeFixture := func(t *testing.T, stringData []byte, neededOffset uint64) (*elf.File, *os.File) {
		t.Helper()
		dynamicSize := uint64(64)
		stringOffset := dynamicSize
		data := make([]byte, int(stringOffset)+len(stringData))
		putDynamicEntry(data[0:16], elf.DT_STRTAB, 0x1000+stringOffset)
		putDynamicEntry(data[16:32], elf.DT_STRSZ, uint64(len(stringData)))
		putDynamicEntry(data[32:48], elf.DT_NEEDED, neededOffset)
		putDynamicEntry(data[48:64], elf.DT_NULL, 0)
		copy(data[stringOffset:], stringData)
		path := writeFile(t, "dynamic-data", data)
		source, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		file := &elf.File{
			FileHeader: elf.FileHeader{Class: elf.ELFCLASS64, Data: elf.ELFDATA2LSB, Type: elf.ET_DYN, Machine: elf.EM_AARCH64},
			Progs: []*elf.Prog{
				{ProgHeader: elf.ProgHeader{Type: elf.PT_LOAD, Off: 0, Vaddr: 0x1000, Filesz: uint64(len(data))}},
				{ProgHeader: elf.ProgHeader{Type: elf.PT_DYNAMIC, Off: 0, Vaddr: 0x1000, Filesz: dynamicSize}},
			},
			Sections: []*elf.Section{
				{SectionHeader: elf.SectionHeader{Type: elf.SHT_DYNAMIC, Offset: 0, Addr: 0x1000, Size: dynamicSize, Entsize: 16, Link: 1}},
				{SectionHeader: elf.SectionHeader{Type: elf.SHT_STRTAB, Offset: stringOffset, Addr: 0x1000 + stringOffset, Size: uint64(len(stringData))}},
			},
		}
		return file, source
	}

	valid, source := makeFixture(t, []byte("libc.so\x00"), 0)
	valid.Sections[0].Offset = 128
	if _, _, _, _, _, _, err := dynamicStrings(context.Background(), valid, source); err == nil || !strings.Contains(err.Error(), "PT_DYNAMIC") {
		t.Fatalf("decoy dynamic section accepted: %v", err)
	}
	source.Close()

	long, source := makeFixture(t, append([]byte(strings.Repeat("x", maxDynamicString+1)), 0), 0)
	info, _ := source.Stat()
	facts, err := extractFacts(context.Background(), long, source, structuralHeader{elfType: uint16(elf.ET_DYN), size: info.Size()})
	source.Close()
	var classified *factError
	if !errors.As(err, &classified) || classified.code != "dynamic_metadata_limit" || facts.OmissionCountsKnown {
		t.Fatalf("long dynamic string facts=%+v error=%v", facts, err)
	}
}

func putDynamicEntry(destination []byte, tag elf.DynTag, value uint64) {
	binary.LittleEndian.PutUint64(destination[:8], uint64(tag))
	binary.LittleEndian.PutUint64(destination[8:16], value)
}

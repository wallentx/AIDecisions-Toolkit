package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"unicode/utf8"
)

const (
	maxProgramHeaders             = 2048
	maxSectionHeaders             = 4096
	maxDynamicBytes               = 1 << 20
	maxDynamicStrings             = 256
	maxDynamicString              = 4096
	maxRetainedDynamicStringBytes = 64 << 10
)

var errParserPanic = errors.New("debug/elf panic")

type openedFile struct {
	file     *os.File
	absPath  string
	realPath string
	info     os.FileInfo
}

type structuralHeader struct {
	elfType uint16
	size    int64
}

func defaults(options Options) Options {
	if options.MaxFileBytes == 0 {
		options.MaxFileBytes = DefaultMaxFileBytes
	}
	return options
}

func ValidateOptions(options Options) error {
	options = defaults(options)
	if options.MaxFileBytes < 1 || options.MaxFileBytes > MaximumMaxFileBytes {
		return fmt.Errorf("max-file-bytes must be between 1 and %d", MaximumMaxFileBytes)
	}
	return nil
}

// PreflightEnvelope rejects path-heavy requests that cannot preserve mandatory
// provenance and allowlist entries. It does not stat or read the target or
// observe host paths.
func PreflightEnvelope(targetPath string, manifest *HostManifest, options Options) error {
	if err := ValidateOptions(options); err != nil {
		return err
	}
	abs, err := filepath.Abs(targetPath)
	if err != nil {
		return err
	}
	if manifestToolVersion() == "" {
		return errors.New("invalid embedded runtime manifest")
	}
	report := baseReport(abs)
	report.Target.ResolvedPath = abs
	if manifest != nil {
		report.Host.Observation = "observed"
		report.Host.OS, report.Host.Arch = goruntime.GOOS, goruntime.GOARCH
		for _, path := range manifest.Paths {
			report.Host.Paths = append(report.Host.Paths, PathObservation{Path: path, State: "unreadable", Kind: "symlink"})
		}
	}
	encoded, err := encodeReport(report)
	if err != nil {
		return err
	}
	if len(encoded) > MaximumOutputBytes {
		return errors.New("mandatory provenance and host manifest paths exceed the fixed 4096-byte output envelope")
	}
	return nil
}

func baseReport(targetPath string) Report {
	return Report{
		SchemaVersion: SchemaVersion,
		Status:        "unavailable",
		Target:        Target{Path: targetPath, Stability: Stability{BestEffort: true}},
		Host:          HostFacts{Observation: "unchecked", Paths: []PathObservation{}},
		Provenance: Provenance{ReadOnly: true, Atomic: false, Parser: ParserIdentity{
			Name: "go/debug/elf", GoVersion: goruntime.Version(), ToolVersion: manifestToolVersion(),
		}},
		Unknowns: append([]string(nil), fixedUnknowns...),
		Output:   Output{MaxBytes: MaximumOutputBytes, OmissionCountKnown: true},
	}
}

func manifestToolVersion() string {
	var metadata struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(ManifestJSON, &metadata) != nil {
		return ""
	}
	return metadata.Version
}

func issue(report *Report, status, code, message string) (Report, error) {
	report.Status, report.Complete, report.Source.Complete = status, false, false
	report.Issue = &Issue{Code: code, Message: message}
	return *report, nil
}

// Collect gathers facts in-process. It never executes or modifies targetPath.
func Collect(ctx context.Context, targetPath string, manifest *HostManifest, options Options) (Report, error) {
	return collect(ctx, targetPath, manifest, options, nil)
}

func collect(ctx context.Context, targetPath string, manifest *HostManifest, options Options, afterParse func()) (Report, error) {
	options = defaults(options)
	if err := ValidateOptions(options); err != nil {
		return Report{}, err
	}
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return Report{}, err
	}
	report := baseReport(absTarget)
	if err := PreflightEnvelope(absTarget, manifest, options); err != nil {
		return Report{}, err
	}
	target, err := openRegular(absTarget, options.MaxFileBytes, "target")
	if err != nil {
		return issue(&report, "unavailable", "target_unavailable", safeOpenMessage("target", err))
	}
	defer target.file.Close()
	report.Target.ResolvedPath, report.Target.SizeBytes = target.realPath, target.info.Size()

	beforeHash, err := hashFile(ctx, target.file, target.info.Size())
	if err != nil {
		return issue(&report, "incomplete", contextCode(ctx, "target_hash_failed"), "target could not be hashed within the configured operation boundary")
	}
	report.Target.SHA256 = beforeHash
	header, status, code, err := validateStructure(target.file, target.info.Size())
	if err != nil {
		return issue(&report, status, code, err.Error())
	}

	section := io.NewSectionReader(target.file, 0, header.size)
	parsed, err := safeNewELF(section)
	if err != nil {
		if errors.Is(err, errParserPanic) {
			return issue(&report, "incomplete", "parser_panic", "in-process ELF parser stopped at a malformed structure")
		}
		return issue(&report, "malformed", "malformed_elf", "target ELF structures could not be parsed")
	}
	defer parsed.Close()
	facts, err := safeExtractFacts(ctx, parsed, target.file, header)
	var partial *factError
	if err != nil {
		var classified *factError
		if errors.As(err, &classified) {
			if facts.Format != "" {
				report.ELF = &facts
				if !facts.OmissionCountsKnown {
					report.Output.OmissionCountKnown = false
				}
				partial = classified
			} else {
				return issue(&report, classified.status, classified.code, classified.message)
			}
		} else {
			return issue(&report, "malformed", "malformed_elf", err.Error())
		}
	} else {
		report.ELF = &facts
	}
	observeHost(&report, manifest)
	if facts.Interpreter == "" {
		report.ELF.InterpreterObservation = "not_declared"
	} else {
		report.ELF.InterpreterObservation = lookupPathState(report.Host.Paths, facts.Interpreter)
	}
	if afterParse != nil {
		afterParse()
	}
	if err := ctx.Err(); err != nil {
		return issue(&report, "incomplete", "timeout", "collection exceeded the configured timeout")
	}

	afterInfo, statErr := target.file.Stat()
	afterHash, hashErr := hashFile(ctx, target.file, header.size)
	pathInfo, pathErr := os.Stat(target.absPath)
	if ctx.Err() != nil {
		return issue(&report, "incomplete", "timeout", "collection exceeded the configured timeout")
	}
	report.Target.AfterSHA256 = afterHash
	report.Target.Stability.IdentityStable = statErr == nil && os.SameFile(target.info, afterInfo)
	report.Target.Stability.SizeStable = statErr == nil && target.info.Size() == afterInfo.Size()
	report.Target.Stability.HashStable = hashErr == nil && beforeHash == afterHash
	report.Target.Stability.PathBindingStable = pathErr == nil && os.SameFile(target.info, pathInfo)
	if statErr != nil || hashErr != nil || pathErr != nil {
		return issue(&report, "incomplete", "stability_unverified", "target stability could not be verified after collection")
	}
	if !report.Target.Stability.IdentityStable || !report.Target.Stability.SizeStable || !report.Target.Stability.HashStable || !report.Target.Stability.PathBindingStable {
		return issue(&report, "changed", "input_changed", "target identity, size, hash, or path binding changed during collection")
	}
	report.Target.AfterSHA256 = ""
	if partial != nil {
		return issue(&report, partial.status, partial.code, partial.message)
	}
	report.Status, report.Complete, report.Source.Complete = "complete", true, true
	return report, nil
}

func openRegular(path string, maxBytes int64, label string) (*openedFile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	file, err := openNonblocking(real)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%s is not a regular file", label)
	}
	if info.Size() < 0 || info.Size() > maxBytes {
		file.Close()
		return nil, fmt.Errorf("%s size exceeds %d bytes", label, maxBytes)
	}
	binding, err := os.Stat(abs)
	if err != nil || !os.SameFile(info, binding) {
		file.Close()
		return nil, fmt.Errorf("%s path binding changed while opening", label)
	}
	return &openedFile{file: file, absPath: abs, realPath: real, info: info}, nil
}

func hashFile(ctx context.Context, file *os.File, size int64) (string, error) {
	if size < 0 {
		return "", errors.New("negative file size")
	}
	reader := io.NewSectionReader(file, 0, size)
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	remaining := size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		chunk := int64(len(buffer))
		if remaining < chunk {
			chunk = remaining
		}
		n, err := reader.Read(buffer[:chunk])
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
			remaining -= int64(n)
		}
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", io.ErrUnexpectedEOF
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validateStructure(file *os.File, size int64) (structuralHeader, string, string, error) {
	result := structuralHeader{size: size}
	var data [64]byte
	n, err := file.ReadAt(data[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return result, "malformed", "header_read_failed", errors.New("ELF header could not be read")
	}
	if n < 4 {
		return result, "malformed", "truncated_header", errors.New("target is shorter than the required ELF header prefix")
	}
	if string(data[:4]) != "\x7fELF" {
		return result, "unsupported", "not_elf", errors.New("target is not an ELF file")
	}
	if n < 20 {
		return result, "malformed", "truncated_header", errors.New("target is shorter than the required ELF header prefix")
	}
	if data[4] != byte(elf.ELFCLASS64) {
		return result, "unsupported", "unsupported_class", errors.New("target is not ELF64")
	}
	if data[5] != byte(elf.ELFDATA2LSB) {
		return result, "unsupported", "unsupported_endianness", errors.New("target is not little-endian ELF")
	}
	if data[6] != byte(elf.EV_CURRENT) {
		return result, "malformed", "invalid_elf_version", errors.New("target has an invalid ELF identification version")
	}
	if n < len(data) {
		return result, "malformed", "truncated_header", errors.New("target has a truncated ELF64 header")
	}
	result.elfType = binary.LittleEndian.Uint16(data[16:18])
	if binary.LittleEndian.Uint16(data[18:20]) != uint16(elf.EM_AARCH64) {
		return result, "unsupported", "unsupported_machine", errors.New("target machine is not AArch64")
	}
	if result.elfType != uint16(elf.ET_EXEC) && result.elfType != uint16(elf.ET_DYN) {
		return result, "unsupported", "unsupported_elf_type", errors.New("target is not an ELF executable or shared object")
	}
	if binary.LittleEndian.Uint16(data[52:54]) != 64 {
		return result, "malformed", "invalid_header_size", errors.New("target has an invalid ELF64 header size")
	}
	phoff, shoff := binary.LittleEndian.Uint64(data[32:40]), binary.LittleEndian.Uint64(data[40:48])
	phentsize, phnum := uint64(binary.LittleEndian.Uint16(data[54:56])), uint64(binary.LittleEndian.Uint16(data[56:58]))
	shentsize, shnum := uint64(binary.LittleEndian.Uint16(data[58:60])), uint64(binary.LittleEndian.Uint16(data[60:62]))
	shstrndx := uint64(binary.LittleEndian.Uint16(data[62:64]))
	if phnum == 0xffff || shnum == 0 {
		// Zero section headers is valid only when no section table exists. Extended
		// numbering would require trusting section zero before bounds are frozen.
		if phnum == 0xffff || shoff != 0 {
			return result, "unsupported", "extended_header_numbering", errors.New("extended ELF header numbering is outside the v1 parser boundary")
		}
	}
	if phnum > maxProgramHeaders || shnum > maxSectionHeaders {
		return result, "malformed", "header_count_limit", errors.New("ELF header table count exceeds the supported bound")
	}
	if phnum > 0 && (phentsize != 56 || !rangeWithin(phoff, phnum, phentsize, uint64(size))) {
		return result, "malformed", "program_header_bounds", errors.New("ELF program header table is outside the target")
	}
	if shnum > 0 && (shentsize != 64 || !rangeWithin(shoff, shnum, shentsize, uint64(size))) {
		return result, "malformed", "section_header_bounds", errors.New("ELF section header table is outside the target")
	}
	if shstrndx == 0xffff {
		return result, "unsupported", "extended_header_numbering", errors.New("extended ELF string-table numbering is outside the v1 parser boundary")
	}
	if shstrndx >= shnum && shstrndx != 0 {
		return result, "malformed", "section_string_table_bounds", errors.New("ELF section-name string table index is outside the section table")
	}
	if err := validateProgramResources(file, phoff, phnum, phentsize, uint64(size)); err != nil {
		return result, "malformed", "program_resource_bounds", err
	}
	if err := validateSectionResources(file, shoff, shnum, shentsize, shstrndx, uint64(size)); err != nil {
		return result, "malformed", "section_resource_limit", err
	}
	return result, "", "", nil
}

func rangeWithin(offset, count, width, size uint64) bool {
	if offset > size || (count != 0 && width > (^uint64(0)-offset)/count) {
		return false
	}
	return offset+count*width <= size
}

func validateProgramResources(file *os.File, offset, count, width, fileSize uint64) error {
	if count == 0 {
		return nil
	}
	entry := make([]byte, width)
	for index := uint64(0); index < count; index++ {
		if _, err := file.ReadAt(entry, int64(offset+index*width)); err != nil {
			return errors.New("ELF program header could not be read")
		}
		programOffset := binary.LittleEndian.Uint64(entry[8:16])
		programSize := binary.LittleEndian.Uint64(entry[32:40])
		if !rangeWithin(programOffset, 1, programSize, fileSize) {
			return errors.New("ELF program segment lies outside the target")
		}
	}
	return nil
}

func validateSectionResources(file *os.File, offset, count, width, shstrndx, fileSize uint64) error {
	if count == 0 {
		return nil
	}
	entry := make([]byte, width)
	for index := uint64(0); index < count; index++ {
		if _, err := file.ReadAt(entry, int64(offset+index*width)); err != nil {
			return errors.New("ELF section header could not be read")
		}
		typeValue := elf.SectionType(binary.LittleEndian.Uint32(entry[4:8]))
		flags := elf.SectionFlag(binary.LittleEndian.Uint64(entry[8:16]))
		sectionOffset := binary.LittleEndian.Uint64(entry[24:32])
		sectionSize := binary.LittleEndian.Uint64(entry[32:40])
		if typeValue != elf.SHT_NOBITS && !rangeWithin(sectionOffset, 1, sectionSize, fileSize) {
			return errors.New("ELF section lies outside the target")
		}
		if typeValue == elf.SHT_DYNAMIC || typeValue == elf.SHT_STRTAB {
			if sectionSize > maxDynamicBytes {
				return errors.New("ELF dynamic or string section exceeds 1 MiB")
			}
			if flags&elf.SHF_COMPRESSED != 0 {
				return errors.New("compressed ELF dynamic or string sections are unsupported")
			}
		}
		if index == shstrndx && shstrndx != 0 && typeValue != elf.SHT_STRTAB {
			return errors.New("ELF section-name table is not a string table")
		}
	}
	return nil
}

type factError struct {
	status, code, message string
}

func (err *factError) Error() string { return err.message }

func safeNewELF(reader io.ReaderAt) (result *elf.File, resultErr error) {
	defer func() {
		if recover() != nil {
			result, resultErr = nil, errParserPanic
		}
	}()
	return elf.NewFile(reader)
}

func safeExtractFacts(ctx context.Context, file *elf.File, source *os.File, header structuralHeader) (facts ELFFacts, resultErr error) {
	defer func() {
		if recover() != nil {
			facts = ELFFacts{}
			resultErr = &factError{status: "incomplete", code: "parser_panic", message: "in-process ELF parser stopped at a malformed structure"}
		}
	}()
	return extractFacts(ctx, file, source, header)
}

func extractFacts(ctx context.Context, file *elf.File, source *os.File, header structuralHeader) (ELFFacts, error) {
	if file.Class != elf.ELFCLASS64 || file.Data != elf.ELFDATA2LSB || file.Machine != elf.EM_AARCH64 ||
		(file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN) || uint16(file.Type) != header.elfType {
		return ELFFacts{}, errors.New("parsed ELF header disagrees with the bounded structural header")
	}
	typeName := "executable"
	if file.Type == elf.ET_DYN {
		typeName = "shared_object"
	}
	interpreter := ""
	for _, program := range file.Progs {
		if program.Type != elf.PT_INTERP {
			continue
		}
		if interpreter != "" {
			return ELFFacts{}, errors.New("target has duplicate PT_INTERP program headers")
		}
		if program.Filesz < 2 || program.Filesz > 4096 || !rangeWithin(program.Off, 1, program.Filesz, uint64(header.size)) {
			return ELFFacts{}, errors.New("PT_INTERP has an invalid bounded file range")
		}
		data := make([]byte, int(program.Filesz))
		if _, err := source.ReadAt(data, int64(program.Off)); err != nil {
			return ELFFacts{}, errors.New("PT_INTERP could not be read from target")
		}
		if data[len(data)-1] != 0 || strings.IndexByte(string(data[:len(data)-1]), 0) >= 0 || !utf8.Valid(data[:len(data)-1]) || data[0] != '/' {
			return ELFFacts{}, errors.New("PT_INTERP is not one valid absolute NUL-terminated UTF-8 path")
		}
		interpreter = string(data[:len(data)-1])
	}
	needed, rpath, runpath, omittedNeeded, omittedRPath, omittedRunPath, err := dynamicStrings(ctx, file, source)
	facts := ELFFacts{Format: "ELF", Machine: "AArch64", Bitness: 64, Endianness: "little", Type: typeName,
		Interpreter: interpreter, Needed: nonnil(needed), RPath: nonnil(rpath), RunPath: nonnil(runpath),
		OmittedNeeded: omittedNeeded, OmittedRPath: omittedRPath, OmittedRunPath: omittedRunPath, OmissionCountsKnown: true}
	if err != nil {
		var classified *factError
		if errors.As(err, &classified) && classified.code == "dynamic_metadata_limit" {
			facts.OmissionCountsKnown = false
		}
		return facts, err
	}
	return facts, nil
}

func dynamicStrings(ctx context.Context, file *elf.File, source *os.File) (needed, rpath, runpath []string, omittedNeeded, omittedRPath, omittedRunPath int, resultErr error) {
	needed, rpath, runpath = []string{}, []string{}, []string{}
	var dynamicProgram *elf.Prog
	dynamicProgramCount := 0
	for _, program := range file.Progs {
		if program.Type == elf.PT_DYNAMIC {
			dynamicProgram, dynamicProgramCount = program, dynamicProgramCount+1
		}
	}
	var dynamic *elf.Section
	for _, section := range file.Sections {
		if section.Type != elf.SHT_DYNAMIC {
			continue
		}
		if dynamic != nil {
			return nil, nil, nil, 0, 0, 0, errors.New("target has multiple dynamic sections")
		}
		dynamic = section
	}
	if dynamic == nil {
		if dynamicProgramCount != 0 {
			return nil, nil, nil, 0, 0, 0, &factError{status: "incomplete", code: "dynamic_metadata_unavailable", message: "PT_DYNAMIC exists without a bounded dynamic section"}
		}
		return needed, rpath, runpath, 0, 0, 0, nil
	}
	if dynamicProgramCount != 1 || dynamicProgram == nil || dynamic.Offset != dynamicProgram.Off || dynamic.Addr != dynamicProgram.Vaddr || dynamic.Size != dynamicProgram.Filesz {
		return nil, nil, nil, 0, 0, 0, errors.New("dynamic section does not bind exactly to one PT_DYNAMIC segment")
	}
	if dynamic.Flags&elf.SHF_COMPRESSED != 0 || strings.HasPrefix(dynamic.Name, ".zdebug") || dynamic.Size > maxDynamicBytes || dynamic.Size%16 != 0 || dynamic.Entsize != 16 {
		return nil, nil, nil, 0, 0, 0, errors.New("dynamic section violates the bounded uncompressed metadata contract")
	}
	if int(dynamic.Link) >= len(file.Sections) {
		return nil, nil, nil, 0, 0, 0, errors.New("dynamic section has an invalid linked string table")
	}
	stringsSection := file.Sections[dynamic.Link]
	if stringsSection.Type != elf.SHT_STRTAB || stringsSection.Flags&elf.SHF_COMPRESSED != 0 || strings.HasPrefix(stringsSection.Name, ".zdebug") || stringsSection.Size > maxDynamicBytes {
		return nil, nil, nil, 0, 0, 0, errors.New("dynamic string table violates the bounded uncompressed metadata contract")
	}
	entry := make([]byte, 16)
	foundNull := false
	var stringAddress, stringSize uint64
	hasStringAddress, hasStringSize, hasStringReferences := false, false, false
	for offset := uint64(0); offset < dynamic.Size; offset += 16 {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, 0, 0, 0, &factError{status: "incomplete", code: "timeout", message: "collection exceeded the configured timeout"}
		}
		if _, err := source.ReadAt(entry, int64(dynamic.Offset+offset)); err != nil {
			return nil, nil, nil, 0, 0, 0, errors.New("dynamic entry could not be read")
		}
		tag := elf.DynTag(int64(binary.LittleEndian.Uint64(entry[:8])))
		value := binary.LittleEndian.Uint64(entry[8:16])
		if tag == elf.DT_NULL {
			foundNull = true
			break
		}
		switch tag {
		case elf.DT_STRTAB:
			if hasStringAddress {
				return nil, nil, nil, 0, 0, 0, errors.New("dynamic section repeats DT_STRTAB")
			}
			hasStringAddress, stringAddress = true, value
		case elf.DT_STRSZ:
			if hasStringSize {
				return nil, nil, nil, 0, 0, 0, errors.New("dynamic section repeats DT_STRSZ")
			}
			hasStringSize, stringSize = true, value
		case elf.DT_NEEDED, elf.DT_RPATH, elf.DT_RUNPATH:
			hasStringReferences = true
		}
	}
	if !foundNull {
		return nil, nil, nil, 0, 0, 0, errors.New("dynamic section has no bounded DT_NULL terminator")
	}
	if hasStringAddress != hasStringSize || (hasStringReferences && !hasStringAddress) {
		return nil, nil, nil, 0, 0, 0, errors.New("dynamic string metadata is missing or inconsistent")
	}
	if hasStringAddress {
		if stringAddress != stringsSection.Addr || stringSize != stringsSection.Size || !mapsThroughOneLoad(file, stringAddress, stringSize, stringsSection.Offset) {
			return nil, nil, nil, 0, 0, 0, errors.New("linked dynamic string table does not match DT_STRTAB, DT_STRSZ, and one PT_LOAD mapping")
		}
	}
	stringData := make([]byte, int(stringsSection.Size))
	if _, err := source.ReadAt(stringData, int64(stringsSection.Offset)); err != nil && !(errors.Is(err, io.EOF) && len(stringData) == 0) {
		return nil, nil, nil, 0, 0, 0, errors.New("dynamic string table could not be read")
	}

	foundNull = false
	retainedCount, retainedBytes := 0, 0
	for offset := uint64(0); offset < dynamic.Size; offset += 16 {
		if err := ctx.Err(); err != nil {
			return needed, rpath, runpath, omittedNeeded, omittedRPath, omittedRunPath, &factError{status: "incomplete", code: "timeout", message: "collection exceeded the configured timeout"}
		}
		if _, err := source.ReadAt(entry, int64(dynamic.Offset+offset)); err != nil {
			return nil, nil, nil, 0, 0, 0, errors.New("dynamic entry could not be read")
		}
		tag := elf.DynTag(int64(binary.LittleEndian.Uint64(entry[:8])))
		value := binary.LittleEndian.Uint64(entry[8:16])
		if tag == elf.DT_NULL {
			foundNull = true
			break
		}
		if tag != elf.DT_NEEDED && tag != elf.DT_RPATH && tag != elf.DT_RUNPATH {
			continue
		}
		if retainedCount >= maxDynamicStrings || retainedBytes >= maxRetainedDynamicStringBytes {
			return needed, rpath, runpath, 0, 0, 0, &factError{status: "incomplete", code: "dynamic_metadata_limit", message: "declared dynamic strings exceeded the retained count or byte boundary"}
		}
		textBytes, err := boundedCString(stringData, value, maxRetainedDynamicStringBytes-retainedBytes)
		if err != nil {
			return nil, nil, nil, 0, 0, 0, err
		}
		components := 1
		if tag == elf.DT_RPATH || tag == elf.DT_RUNPATH {
			components = bytes.Count(textBytes, []byte{':'}) + 1
		}
		if retainedCount+components > maxDynamicStrings {
			return needed, rpath, runpath, 0, 0, 0, &factError{status: "incomplete", code: "dynamic_metadata_limit", message: "declared dynamic strings exceeded the retained count or byte boundary"}
		}
		text := string(textBytes)
		retainedCount += components
		retainedBytes += len(textBytes)
		switch tag {
		case elf.DT_NEEDED:
			needed = append(needed, text)
		case elf.DT_RPATH:
			rpath = append(rpath, strings.Split(text, ":")...)
		case elf.DT_RUNPATH:
			runpath = append(runpath, strings.Split(text, ":")...)
		}
	}
	return needed, rpath, runpath, 0, 0, 0, nil
}

func mapsThroughOneLoad(file *elf.File, address, size, fileOffset uint64) bool {
	matches := 0
	for _, program := range file.Progs {
		if program.Type != elf.PT_LOAD || address < program.Vaddr {
			continue
		}
		delta := address - program.Vaddr
		if delta <= program.Filesz && size <= program.Filesz-delta && program.Off <= ^uint64(0)-delta && program.Off+delta == fileOffset {
			matches++
		}
	}
	return matches == 1
}

func boundedCString(table []byte, offset uint64, aggregateRemaining int) ([]byte, error) {
	if offset >= uint64(len(table)) {
		return nil, errors.New("dynamic string offset is outside the linked table")
	}
	limit := maxDynamicString
	if aggregateRemaining < limit {
		limit = aggregateRemaining
	}
	available := len(table) - int(offset)
	window := available
	if window > limit+1 {
		window = limit + 1
	}
	end := bytes.IndexByte(table[int(offset):int(offset)+window], 0)
	if end < 0 {
		if available <= limit {
			return nil, errors.New("dynamic string is not NUL-terminated within the linked table")
		}
		return nil, &factError{status: "incomplete", code: "dynamic_metadata_limit", message: "a declared dynamic string exceeded the 4096-byte or aggregate boundary"}
	}
	if end > limit {
		return nil, &factError{status: "incomplete", code: "dynamic_metadata_limit", message: "a declared dynamic string exceeded the 4096-byte or aggregate boundary"}
	}
	value := table[int(offset) : int(offset)+end]
	if !utf8.Valid(value) {
		return nil, errors.New("dynamic string is not valid UTF-8")
	}
	return value, nil
}

func nonnil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func observeHost(report *Report, manifest *HostManifest) {
	if manifest == nil || !manifest.ObserveHost {
		return
	}
	report.Host.Observation, report.Host.OS, report.Host.Arch = "observed", goruntime.GOOS, goruntime.GOARCH
	for _, path := range manifest.Paths {
		observation := PathObservation{Path: path}
		linkInfo, linkErr := os.Lstat(path)
		if linkErr == nil {
			observation.Kind = fileKind(linkInfo.Mode())
		}
		_, err := os.Stat(path)
		switch {
		case err == nil:
			observation.State = "present"
		case errors.Is(err, os.ErrNotExist), errors.Is(linkErr, os.ErrNotExist):
			observation.State = "missing"
		default:
			observation.State = "unreadable"
		}
		report.Host.Paths = append(report.Host.Paths, observation)
	}
}

func fileKind(mode os.FileMode) string {
	switch {
	case mode&os.ModeSymlink != 0:
		return "symlink"
	case mode.IsRegular():
		return "regular"
	case mode.IsDir():
		return "directory"
	default:
		return "other"
	}
}

func lookupPathState(paths []PathObservation, path string) string {
	for _, observation := range paths {
		if observation.Path == path {
			return observation.State
		}
	}
	return "unchecked"
}

func safeOpenMessage(label string, err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return label + " does not exist"
	case errors.Is(err, os.ErrPermission):
		return label + " is unreadable"
	default:
		return label + " is unavailable or violates the regular-file boundary"
	}
}

func contextCode(ctx context.Context, fallback string) string {
	if ctx.Err() != nil {
		return "timeout"
	}
	return fallback
}

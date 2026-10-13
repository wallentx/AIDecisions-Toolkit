// Package runtime collects bounded, static facts about one AArch64 ELF file.
package runtime

import "aid/internal/evidence"

const (
	SchemaVersion       = 1
	DefaultMaxFileBytes = int64(512 << 20)
	MaximumMaxFileBytes = int64(2 << 30)
	MaximumOutputBytes  = 4 << 10
)

var fixedUnknowns = []string{
	"dependency_resolution",
	"child_launch_environment",
	"execution_policy",
	"actual_launch_success",
}

// Options bounds read-only in-process file work. The zero value receives the
// documented defaults.
type Options struct {
	MaxFileBytes int64
}

type Report struct {
	SchemaVersion int        `json:"schema_version"`
	Status        string     `json:"status"`
	Complete      bool       `json:"complete"`
	Target        Target     `json:"target"`
	ELF           *ELFFacts  `json:"elf,omitempty"`
	Host          HostFacts  `json:"host"`
	Provenance    Provenance `json:"provenance"`
	Unknowns      []string   `json:"unknowns"`
	Source        Source     `json:"source"`
	Issue         *Issue     `json:"issue,omitempty"`
	Output        Output     `json:"output"`
}

type Target struct {
	Path         string    `json:"path"`
	ResolvedPath string    `json:"resolved_path,omitempty"`
	SizeBytes    int64     `json:"size_bytes,omitempty"`
	SHA256       string    `json:"sha256,omitempty"`
	AfterSHA256  string    `json:"after_sha256,omitempty"`
	Stability    Stability `json:"stability"`
}

type Stability struct {
	BestEffort        bool `json:"best_effort"`
	IdentityStable    bool `json:"identity_stable"`
	SizeStable        bool `json:"size_stable"`
	HashStable        bool `json:"hash_stable"`
	PathBindingStable bool `json:"path_binding_stable"`
}

type ELFFacts struct {
	Format                 string   `json:"format"`
	Machine                string   `json:"machine"`
	Bitness                int      `json:"bitness"`
	Endianness             string   `json:"endianness"`
	Type                   string   `json:"type"`
	Interpreter            string   `json:"interpreter,omitempty"`
	InterpreterObservation string   `json:"interpreter_observation"`
	Needed                 []string `json:"needed"`
	RPath                  []string `json:"rpath"`
	RunPath                []string `json:"runpath"`
	OmittedNeeded          int      `json:"omitted_needed,omitempty"`
	OmittedRPath           int      `json:"omitted_rpath,omitempty"`
	OmittedRunPath         int      `json:"omitted_runpath,omitempty"`
	OmissionCountsKnown    bool     `json:"omission_counts_known"`
}

type HostFacts struct {
	Observation  string            `json:"observation"`
	OS           string            `json:"os,omitempty"`
	Arch         string            `json:"arch,omitempty"`
	Paths        []PathObservation `json:"paths"`
	OmittedPaths int               `json:"omitted_paths,omitempty"`
}

type PathObservation struct {
	Path  string `json:"path"`
	State string `json:"state"`
	Kind  string `json:"kind,omitempty"`
}

type Provenance struct {
	ReadOnly bool           `json:"read_only"`
	Atomic   bool           `json:"atomic"`
	Parser   ParserIdentity `json:"parser"`
}

type ParserIdentity struct {
	Name        string `json:"name"`
	GoVersion   string `json:"go_version"`
	ToolVersion string `json:"tool_version"`
}

type Source struct {
	Complete bool `json:"complete"`
}

type Issue = evidence.Error

type Output struct {
	MaxBytes           int  `json:"max_bytes"`
	OmittedItems       int  `json:"omitted_items"`
	OmissionCountKnown bool `json:"omission_count_known"`
	Truncated          bool `json:"truncated"`
}

type HostManifest struct {
	SchemaVersion int      `json:"schema_version"`
	ObserveHost   bool     `json:"observe_host"`
	Paths         []string `json:"paths"`
}

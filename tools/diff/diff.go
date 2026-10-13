// Package diff evaluates code changes using configurable predicate checks.
package diff

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"aid/internal/decisions"
)

const (
	maxDiffBytes    = 1 << 20
	maxContextBytes = 1 << 20
	maxChecksBytes  = 256 << 10
)

//go:embed tool.json
var ManifestJSON []byte

//go:embed SKILL.md
var Skill []byte

//go:embed checks.json
var defaultChecksJSON []byte

//go:embed examples/change.diff
var ExampleDiff string

// version reads the tool manifest so the launcher and executable share one
// independently versioned source of truth.
func version() (string, error) {
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(ManifestJSON, &manifest); err != nil {
		return "", fmt.Errorf("read embedded tool manifest: %w", err)
	}
	if manifest.Version == "" {
		return "", errors.New("embedded tool manifest has no version")
	}
	return manifest.Version, nil
}

// DefaultChecks returns an independent copy of the bundled predicates.
func DefaultChecks() decisions.Checks {
	var checks decisions.Checks
	if err := json.Unmarshal(defaultChecksJSON, &checks); err != nil {
		panic(err) // Embedded source is checked by tests and cannot change at runtime.
	}
	return checks
}

// BuildRequest prepares a code assessment without reading credentials or using
// the network. Callers decide how to use each probability or refusal.
func BuildRequest(change, contextText, model string, checks decisions.Checks) (decisions.Request, error) {
	if len(change) > maxDiffBytes {
		return decisions.Request{}, errors.New("diff exceeds 1 MiB")
	}
	if len(contextText) > maxContextBytes {
		return decisions.Request{}, errors.New("context exceeds 1 MiB")
	}
	if strings.TrimSpace(change) == "" {
		return decisions.Request{}, errors.New("provide a nonempty diff")
	}
	if checks == nil {
		checks = DefaultChecks()
	}
	input, err := json.Marshal(map[string]string{"diff": change, "context": contextText})
	if err != nil {
		return decisions.Request{}, err
	}
	return decisions.BuildRequest(string(input), model, checks)
}

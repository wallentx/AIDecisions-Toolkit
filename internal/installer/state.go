package installer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type record struct {
	Provider string `json:"provider"`
	Tool     string `json:"tool"`
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
}

type mcpRecord struct {
	Provider string `json:"provider"`
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
}

type manifest struct {
	Version int         `json:"version"`
	Files   []record    `json:"files"`
	MCP     []mcpRecord `json:"mcp,omitempty"`
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// checkPath rejects symlinks at every existing component, including the file.
// Nonexistent suffixes are safe to create after the same check under the lock.
func checkPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("path must be absolute and clean: %s", path)
	}
	current := path
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing symlink: %s", current)
			}
			if current != path && !info.IsDir() {
				return fmt.Errorf("parent is not a directory: %s", current)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func readRegular(path string) ([]byte, os.FileMode, bool, error) {
	if err := checkPath(path); err != nil {
		return nil, 0, false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("not a regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	return data, info.Mode().Perm(), true, err
}

func readManifest(path string) (manifest, []byte, error) {
	data, _, exists, err := readRegular(path)
	if err != nil {
		return manifest{}, nil, err
	}
	m := manifest{Version: 1}
	if !exists {
		return m, nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, nil, fmt.Errorf("read installer state %s: %w", path, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return m, nil, fmt.Errorf("unexpected content after installer state in %s", path)
	}
	if m.Version != 1 {
		return m, nil, fmt.Errorf("unsupported installer state version %d", m.Version)
	}
	seen := map[string]bool{}
	for _, r := range m.Files {
		if seen[r.Path] || len(r.SHA256) != 64 {
			return m, nil, fmt.Errorf("invalid installer record for %s", r.Path)
		}
		if _, err := hex.DecodeString(r.SHA256); err != nil {
			return m, nil, fmt.Errorf("invalid installer digest for %s", r.Path)
		}
		seen[r.Path] = true
	}
	seenMCP := map[string]bool{}
	for _, r := range m.MCP {
		if seenMCP[r.Path] || seen[r.Path] || len(r.SHA256) != 64 {
			return m, nil, fmt.Errorf("invalid installer MCP record for %s", r.Path)
		}
		if _, err := hex.DecodeString(r.SHA256); err != nil {
			return m, nil, fmt.Errorf("invalid installer MCP digest for %s", r.Path)
		}
		seenMCP[r.Path] = true
	}
	return m, data, nil
}

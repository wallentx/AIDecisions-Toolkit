package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"aid/internal/bounded"
)

const maximumManifestBytes = 16 << 10

func LoadHostManifest(path string) (*HostManifest, error) {
	if path == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve host manifest path: %w", err)
	}
	file, err := openNonblocking(abs)
	if err != nil {
		return nil, fmt.Errorf("read host manifest: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat host manifest: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("host manifest must be a regular file")
	}
	if info.Size() > maximumManifestBytes {
		return nil, fmt.Errorf("host manifest exceeds %d bytes: %w", maximumManifestBytes, bounded.ErrLimit)
	}
	data, err := bounded.Read(file, maximumManifestBytes, "host manifest")
	if err != nil {
		return nil, fmt.Errorf("read host manifest: %w", err)
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, fmt.Errorf("invalid host manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest HostManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("invalid host manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("host manifest must contain exactly one JSON value")
	}
	if manifest.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported host manifest schema_version %d", manifest.SchemaVersion)
	}
	if !manifest.ObserveHost && len(manifest.Paths) != 0 {
		return nil, errors.New("host manifest paths require observe_host=true")
	}
	if len(manifest.Paths) > 16 {
		return nil, errors.New("host manifest paths exceeds 16 entries")
	}
	seen := make(map[string]bool, len(manifest.Paths))
	for _, path := range manifest.Paths {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, fmt.Errorf("host manifest path must be a clean absolute path: %q", path)
		}
		if seen[path] {
			return nil, fmt.Errorf("duplicate host manifest path: %q", path)
		}
		seen[path] = true
	}
	return &manifest, nil
}

// rejectDuplicateJSONKeys validates every object, not only the top-level
// manifest, so future nested schema additions inherit the same strictness.
func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if seen[key] {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("unexpected JSON delimiter")
		}
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

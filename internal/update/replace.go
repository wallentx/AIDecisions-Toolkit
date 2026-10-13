package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

type artifact struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Remove bool   `json:"remove,omitempty"`
}

var commandPattern = regexp.MustCompile(`^AID(?:\.[a-z][a-z0-9-]*)?(?:\.exe)?$`)

func copyFile(source, target string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", source)
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

// replaceArtifacts stages everything on the destination filesystem before any
// replacement. Renames are atomic where the OS supports it; a failed rename
// rolls back prior renames.
// The suite launcher is replaced last so it does not advertise an incomplete set.
func replaceArtifacts(installDir string, artifacts []artifact, rename func(string, string) error) error {
	if len(artifacts) == 0 {
		return errors.New("no built artifacts to install")
	}
	ordered := append([]artifact(nil), artifacts...)
	seen := make(map[string]bool)
	for _, item := range ordered {
		if !commandPattern.MatchString(item.Name) || seen[item.Name] {
			return fmt.Errorf("invalid or duplicate binary name %q", item.Name)
		}
		if item.Remove && isLauncher(item.Name) {
			return errors.New("the launcher cannot be removed")
		}
		seen[item.Name] = true
	}
	sort.Slice(ordered, func(i, j int) bool {
		if isLauncher(ordered[i].Name) != isLauncher(ordered[j].Name) {
			return !isLauncher(ordered[i].Name)
		}
		return ordered[i].Name < ordered[j].Name
	})
	if !isLauncher(ordered[len(ordered)-1].Name) {
		return errors.New("built artifacts do not include AID")
	}
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return fmt.Errorf("prepare install directory: %w", err)
	}
	lockPath := filepath.Join(installDir, ".aid-update.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("lock installation (another update may be running): %w", err)
	}
	_ = lock.Close()
	defer os.Remove(lockPath)
	transaction, err := os.MkdirTemp(installDir, ".aid-update-")
	if err != nil {
		return fmt.Errorf("stage update in install directory: %w", err)
	}
	keepBackups := false
	defer func() {
		if !keepBackups {
			_ = os.RemoveAll(transaction)
		}
	}()
	existing := make(map[string]bool)
	for _, item := range ordered {
		destination := filepath.Join(installDir, item.Name)
		info, err := os.Lstat(destination)
		if err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("refusing to replace non-regular file %s", destination)
			}
			existing[item.Name] = true
			if err := copyFile(destination, filepath.Join(transaction, "old-"+item.Name), info.Mode().Perm()); err != nil {
				return fmt.Errorf("back up %s: %w", item.Name, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if item.Remove {
			continue
		}
		if err := copyFile(item.Path, filepath.Join(transaction, "new-"+item.Name), 0755); err != nil {
			return fmt.Errorf("stage %s: %w", item.Name, err)
		}
	}
	for i, item := range ordered {
		source, target := filepath.Join(transaction, "new-"+item.Name), filepath.Join(installDir, item.Name)
		if item.Remove {
			if !existing[item.Name] {
				continue
			}
			source, target = target, filepath.Join(transaction, "removed-"+item.Name)
		}
		if err := rename(source, target); err != nil {
			failure := fmt.Errorf("install %s: %w", item.Name, err)
			for j := i - 1; j >= 0; j-- {
				previous := ordered[j]
				if previous.Remove && !existing[previous.Name] {
					continue
				}
				destination := filepath.Join(installDir, previous.Name)
				var rollbackErr error
				if existing[previous.Name] {
					rollbackErr = rename(filepath.Join(transaction, "old-"+previous.Name), destination)
				} else {
					rollbackErr = os.Remove(destination)
				}
				if rollbackErr != nil {
					keepBackups = true
					failure = errors.Join(failure, fmt.Errorf("restore %s: %w", previous.Name, rollbackErr))
				}
			}
			if keepBackups {
				return fmt.Errorf("%w; recovery files retained at %s", failure, transaction)
			}
			return fmt.Errorf("%w; previous installation restored", failure)
		}
	}
	return nil
}

func isLauncher(name string) bool { return name == "AID" || name == "AID.exe" }

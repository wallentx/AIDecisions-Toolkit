//go:build !windows

package discover

import (
	"errors"
	"os"
)

func secureCacheDirectory(path string, created bool) error {
	if !created {
		return verifyCachePrivacy(path)
	}
	if err := os.Chmod(path, 0700); err != nil {
		return errors.New("cannot make discover cache directory private")
	}
	return verifyCachePrivacy(path)
}
func verifyCachePrivacy(path string) error {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return errors.New("discover cache permissions are not private")
	}
	return nil
}

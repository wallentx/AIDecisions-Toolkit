package update

import (
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

var fullVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

func canonicalVersion(value string) (string, error) {
	if !fullVersion.MatchString(value) {
		return "", fmt.Errorf("invalid version %q; expected major.minor.patch", value)
	}
	version := "v" + strings.TrimPrefix(value, "v")
	if !semver.IsValid(version) {
		return "", fmt.Errorf("invalid semantic version %q", value)
	}
	return version, nil
}

func isNewer(current, latest string) (bool, error) {
	currentVersion, err := canonicalVersion(current)
	if err != nil {
		return false, err
	}
	latestVersion, err := canonicalVersion(latest)
	if err != nil {
		return false, err
	}
	return semver.Compare(latestVersion, currentVersion) > 0, nil
}

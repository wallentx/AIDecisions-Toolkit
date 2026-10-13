//go:build !windows

package update

import (
	"errors"
	"os/exec"
)

func startHelper(_ *exec.Cmd) error {
	return errors.New("detached replacement is only used on Windows")
}
func waitForParent(_ int) error {
	return errors.New("internal replacement helper is only used on Windows")
}

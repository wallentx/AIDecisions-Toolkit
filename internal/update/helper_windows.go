package update

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func startHelper(command *exec.Cmd) error {
	const detachedProcess = 0x00000008
	const newProcessGroup = 0x00000200
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | newProcessGroup}
	return command.Start()
}

func waitForParent(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		// The parent often finishes before the detached helper starts.
		if errors.Is(err, syscall.Errno(87)) { // ERROR_INVALID_PARAMETER: PID is gone.
			return nil
		}
		return fmt.Errorf("open parent process: %w", err)
	}
	defer process.Release()
	done := make(chan error, 1)
	go func() {
		_, err := process.Wait()
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		return fmt.Errorf("AID process %d did not exit within 30 seconds", pid)
	}
}

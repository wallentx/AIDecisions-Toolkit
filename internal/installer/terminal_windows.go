//go:build windows

package installer

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func enableANSI(output *os.File) (func(), error) {
	handle := windows.Handle(output.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return nil, fmt.Errorf("read Windows console mode: %w", err)
	}
	if err := windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return nil, fmt.Errorf("enable ANSI terminal output: %w", err)
	}
	return func() { windows.SetConsoleMode(handle, mode) }, nil
}

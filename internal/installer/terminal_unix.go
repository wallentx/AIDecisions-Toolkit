//go:build !windows

package installer

import "os"

func enableANSI(_ *os.File) (func(), error) { return func() {}, nil }

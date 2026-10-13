//go:build !linux && !android

package runtime

import "errors"

func syscallMkfifo(string) error { return errors.New("unsupported") }

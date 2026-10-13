//go:build !linux && !android

package runtime

import (
	"errors"
	"os"
)

func openNonblocking(string) (*os.File, error) {
	return nil, errors.New("AID.runtime supports only Linux and Android hosts")
}

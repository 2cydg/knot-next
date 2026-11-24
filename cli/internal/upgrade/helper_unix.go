//go:build !windows

package upgrade

import (
	"errors"
	"io"
)

func startHelper(_ transaction, _ string, _ commandIO) error {
	return errors.New("the upgrade helper is only used on Windows")
}

func RunHelper(_ []string, _, _ io.Writer) (int, bool) { return 0, false }

//go:build !linux && !darwin && !windows

package upgrade

import "errors"

func watchProcess(_ int) (processWatch, string, error) {
	return processWatch{}, "", errors.New("process inspection is unsupported on this platform")
}

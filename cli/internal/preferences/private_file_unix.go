//go:build !windows

package preferences

import "os"

func createPrivateTemp(dir string) (*os.File, error) {
	return os.CreateTemp(dir, ".client-*.toml")
}

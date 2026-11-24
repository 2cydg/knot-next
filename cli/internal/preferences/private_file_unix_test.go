//go:build !windows

package preferences

import (
	"os"
	"testing"
)

func assertPrivatePreferences(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0077 != 0 {
		t.Fatalf("client credentials have permissive mode %04o", mode)
	}
}

package preferences

import "testing"

func setTestHome(t *testing.T, path string) {
	t.Helper()
	t.Setenv("HOME", path)
	t.Setenv("USERPROFILE", path)
}

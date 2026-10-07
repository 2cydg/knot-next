package paths

import (
	"path/filepath"
	"testing"
)

func TestDefaultLayoutUsesLegacyXDGPathsAndSeparateDiscovery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	layout, err := DefaultLayout()
	if err != nil {
		t.Fatal(err)
	}
	if layout.ConfigDir != filepath.Join(root, "config", "knot") || layout.StateDir != filepath.Join(root, "state", "knot") {
		t.Fatal("legacy business directories changed")
	}
	if layout.TokenPath != filepath.Join(layout.StateDir, "runtime", "token") || layout.RuntimePath != filepath.Join(layout.StateDir, "runtime", "core.json") {
		t.Fatal("core discovery collides with legacy daemon files")
	}
}

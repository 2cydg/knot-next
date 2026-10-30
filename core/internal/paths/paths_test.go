package paths

import (
	"path/filepath"
	"testing"
)

func TestDefaultLayoutIgnoresXDGPaths(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "ignored-config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "ignored-state"))
	layout, err := DefaultLayout()
	if err != nil {
		t.Fatal(err)
	}
	if layout.ConfigDir != filepath.Join(root, ".config", "knot") || layout.StateDir != filepath.Join(root, ".local", "state", "knot") {
		t.Fatal("legacy business directories changed")
	}
	if layout.ConfigurationFile() != filepath.Join(layout.ConfigDir, "config.toml") {
		t.Fatal("default configuration file changed")
	}
}

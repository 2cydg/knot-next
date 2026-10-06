package crypto

import (
	"path/filepath"
	"testing"

	"knot-core/internal/paths"
)

func TestStatePersistsProviderProbe(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "config"), filepath.Join(root, "state"))
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	provider := NewStaticProvider([]byte("state-test-key"))
	if err := PersistState(layout, provider); err != nil {
		t.Fatalf("persist state: %v", err)
	}
	state, err := LoadState(layout)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.Provider != ProviderLocal {
		t.Fatalf("provider = %q, want %q", state.Provider, ProviderLocal)
	}
	if err := ValidateState(state, provider); err != nil {
		t.Fatalf("validate state: %v", err)
	}
}

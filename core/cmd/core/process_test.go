package main

import (
	"log"
	"os"
	"path/filepath"
	"testing"

	"knot-core/internal/logger"
	"knot-core/internal/paths"
	"knot-core/pkg/crypto"
)

func TestMain(m *testing.M) {
	if os.Getenv("KNOT_TEST_CORE_PROCESS") != "1" {
		os.Exit(m.Run())
	}
	// Run the CLI and lifecycle with a private key per test layout. The key
	// survives a restart without depending on the runner's credential store.
	if err := run(func(layout paths.Layout) (crypto.Provider, error) {
		return crypto.NewLocalProvider(filepath.Join(layout.ConfigDir, "secret.key"))
	}); err != nil {
		log.Fatal(logger.Redact(err.Error()))
	}
}

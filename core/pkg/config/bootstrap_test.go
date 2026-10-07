package config

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"knot-core/internal/paths"
	"knot-core/pkg/crypto"
)

func TestLegacyNoStateBootstrapKeepsCiphertext(t *testing.T) {
	svc, _ := legacyFixture(t, crypto.ProviderLinuxMachine)
	if err := os.Remove(filepath.Join(svc.layout.ConfigDir, ".crypto-state")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(svc.path)
	selected := svc.crypto
	svc.crypto = crypto.NewBootstrapProvider(selected, []crypto.Provider{selected}, nil)
	// Ordinary reads and preview do not fix the missing state.
	if _, err := svc.RuntimeConfig(); err != nil {
		t.Fatal(err)
	}
	_ = svc.MigrationPlan()
	if _, err := os.Stat(filepath.Join(svc.layout.ConfigDir, ".crypto-state")); !os.IsNotExist(err) {
		t.Fatal("read initialized crypto state")
	}
	if err := svc.Initialize(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(svc.path)
	if !bytes.Equal(before, after) {
		t.Fatal("same-backend bootstrap re-encrypted TOML")
	}
	state, err := crypto.LoadState(svc.layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := crypto.ValidateState(state, selected); err != nil {
		t.Fatal(err)
	}
	stateRaw, err := os.ReadFile(filepath.Join(svc.layout.ConfigDir, ".crypto-state"))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := json.MarshalIndent(state, "", "  ")
	if err != nil || !bytes.Equal(stateRaw, append(expected, '\n')) {
		t.Fatal("bootstrap state differs from legacy formatting")
	}
}
func TestBootstrapDecryptFailurePreservesInstallation(t *testing.T) {
	svc, _ := legacyFixture(t, crypto.ProviderLinuxMachine)
	if err := os.Remove(filepath.Join(svc.layout.ConfigDir, ".crypto-state")); err != nil {
		t.Fatal(err)
	}
	wrong := crypto.NewKeyProvider(crypto.ProviderLinuxSecret, bytes.Repeat([]byte{9}, 32), nil)
	svc.crypto = crypto.NewBootstrapProvider(wrong, []crypto.Provider{wrong}, nil)
	before := treeHashes(t, filepath.Dir(svc.layout.ConfigDir))
	if err := svc.Initialize(); err == nil {
		t.Fatal("wrong key accepted")
	}
	if after := treeHashes(t, filepath.Dir(svc.layout.ConfigDir)); len(before) != len(after) {
		t.Fatal("bootstrap failure created files")
	} else {
		for path, hash := range before {
			if after[path] != hash {
				t.Fatal("bootstrap failure changed file")
			}
		}
	}
}

// Child process runs the real bootstrap commit and stops after config replacement
// when the deliberately obstructed state path fails. The parent kills the process
// and recovery completes the encrypted staged transaction on the next start.
func TestBootstrapForcedExitRecovery(t *testing.T) {
	if dir := os.Getenv("KNOT_BOOTSTRAP_CHILD_DIR"); dir != "" {
		layout := paths.NewLayout(dir, filepath.Dir(dir))
		salt := make([]byte, 32)
		for i := range salt {
			salt[i] = byte(i)
		}
		old := crypto.NewKeyProvider(crypto.ProviderLinuxMachine, crypto.DeriveKey("fixture-machine\x001000", salt), nil)
		selected := crypto.NewKeyProvider(crypto.ProviderLinuxSecret, bytes.Repeat([]byte{9}, 32), nil)
		svc := NewService(layout, crypto.NewBootstrapProvider(selected, []crypto.Provider{selected, old}, nil))
		if err := svc.Initialize(); err == nil {
			t.Fatal("state obstruction did not fail")
		}
		fmt.Println("bootstrap-staged")
		time.Sleep(time.Hour)
	}
	svc, hashes := legacyFixture(t, crypto.ProviderLinuxMachine)
	statePath := filepath.Join(svc.layout.ConfigDir, ".crypto-state")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0700); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(svc.path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBootstrapForcedExitRecovery$")
	cmd.Env = append(os.Environ(), "KNOT_BOOTSTRAP_CHILD_DIR="+svc.layout.ConfigDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "bootstrap-staged" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("child failed to stage bootstrap")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := RecoverBootstrap(svc.layout); err != nil {
		t.Fatal(err)
	}
	if err := RecoverBootstrap(svc.layout); err != nil {
		t.Fatal("recovery not idempotent")
	}
	selected := crypto.NewKeyProvider(crypto.ProviderLinuxSecret, bytes.Repeat([]byte{9}, 32), nil)
	svc = NewService(svc.layout, selected)
	cfg, err := svc.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	assertHash(t, cfg.Servers["srv_legacy"].Password, hashes["server_password"])
	state, err := crypto.LoadState(svc.layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := crypto.ValidateState(state, selected); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(svc.path + ".bootstrap.bak")
	if err != nil || !bytes.Equal(backup, original) {
		t.Fatal("original backup missing")
	}
	if marker, err := os.ReadFile(filepath.Join(svc.layout.ConfigDir, ".bootstrap-recovery")); err == nil {
		t.Fatalf("recovery marker was not removed: %d bytes", len(marker))
	}
}

func TestLegacyStartupWithFixedProviderIsReadOnly(t *testing.T) {
	svc, _ := legacyFixture(t, crypto.ProviderLinuxSecret)
	if err := os.Remove(svc.path + ".lock"); err != nil {
		t.Fatal(err)
	}
	before := treeHashes(t, filepath.Dir(svc.layout.ConfigDir))
	if err := svc.Initialize(); err != nil {
		t.Fatal(err)
	}
	after := treeHashes(t, filepath.Dir(svc.layout.ConfigDir))
	if len(before) != len(after) {
		t.Fatal("startup created business/material/lock files")
	}
	for path, hash := range before {
		if after[path] != hash {
			t.Fatal("startup changed installation")
		}
	}
}

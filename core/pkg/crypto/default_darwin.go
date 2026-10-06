//go:build darwin

package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"knot-core/internal/paths"
)

const (
	keychainAccount = "knot-core-master-key"
	keychainService = "knot-core"
)

func providerForState(layout paths.Layout, providerID string) (Provider, error) {
	switch providerID {
	case ProviderDarwinKeychain:
		key, err := getOrCreateKeychainKey()
		if err != nil {
			return nil, err
		}
		return NewKeyProvider(ProviderDarwinKeychain, key, nil), nil
	case ProviderLocal:
		return NewLocalProvider(localKeyPath(layout))
	default:
		return nil, fmt.Errorf("unknown crypto provider %q", providerID)
	}
}

func selectDefaultProvider(layout paths.Layout) (Provider, error) {
	key, err := getOrCreateKeychainKey()
	if err != nil {
		return nil, err
	}
	return NewKeyProvider(ProviderDarwinKeychain, key, nil), nil
}

func getOrCreateKeychainKey() ([]byte, error) {
	out, err := exec.Command("security", "find-generic-password", "-a", keychainAccount, "-s", keychainService, "-w").Output()
	if err == nil {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
		if err == nil && len(key) == 32 {
			return key, nil
		}
		_ = exec.Command("security", "delete-generic-password", "-a", keychainAccount, "-s", keychainService).Run()
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	cmd := exec.Command("security", "add-generic-password", "-a", keychainAccount, "-s", keychainService, "-w", "-")
	cmd.Stdin = strings.NewReader(base64.StdEncoding.EncodeToString(key))
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to store key in macOS Keychain: %w", err)
	}
	return key, nil
}

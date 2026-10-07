//go:build darwin

package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"

	"knot-core/internal/paths"
)

const (
	keychainAccount = "knot-master-key"
	keychainService = "knot"
)

type darwinProvider struct {
	key         []byte
	fallbackKey []byte
}

func openPlatformProvider(layout paths.Layout, initialize bool) (Provider, error) {
	slog.Debug("Initializing macOS Keychain crypto provider")

	// 1. Get Machine ID for fallback
	machineID, err := getMachineID()
	if err != nil {
		slog.Debug("Failed to get IOPlatformUUID", "error", err)
	}

	salt, err := readSalt(layout, allowSaltInitialization(layout, initialize))
	if err != nil {
		return nil, fmt.Errorf("failed to get salt: %w", err)
	}

	var fallbackKey []byte
	if machineID != "" {
		fallbackKey = DeriveKey(machineID, salt)
	}

	// 2. Get or create Keychain key
	key, err := keychainKey(initialize)
	if err != nil {
		slog.Debug("Keychain access failed, will use Machine ID fallback if available", "error", err)
	}

	// 3. Validation
	if key == nil && fallbackKey == nil {
		return nil, fmt.Errorf("failed to initialize any crypto provider on macOS (Keychain failed and Machine ID not found)")
	}

	return &darwinProvider{
		key:         key,
		fallbackKey: fallbackKey,
	}, nil
}

func (p *darwinProvider) Available() bool { return len(p.key) == 32 || len(p.fallbackKey) == 32 }
func (p *darwinProvider) Limitations() []string {
	return []string{"legacy machine fallback is supported"}
}

func (p *darwinProvider) Name() string {
	if p.key == nil {
		return "Machine ID Fallback"
	}
	return "macOS Keychain"
}

func (p *darwinProvider) Encrypt(plaintext []byte) ([]byte, error) {
	key := p.key
	if key == nil {
		if p.fallbackKey == nil {
			return nil, fmt.Errorf("no encryption key available")
		}
		slog.Debug("Encrypting using Machine ID fallback")
		key = p.fallbackKey
	} else {
		slog.Debug("Encrypting using Keychain key")
	}

	return EncryptWithKey(plaintext, key)
}

func (p *darwinProvider) Decrypt(ciphertext []byte) ([]byte, error) {
	// Try main key first (Keychain)
	if p.key != nil {
		slog.Debug("Attempting decryption with Keychain key")
		plaintext, err := DecryptWithKey(ciphertext, p.key)
		if err == nil {
			return plaintext, nil
		}
		slog.Debug("Decryption with Keychain key failed", "error", err)
	}

	// Fallback to machine-id key
	if p.fallbackKey != nil {
		slog.Debug("Attempting decryption with Machine ID fallback key")
		return DecryptWithKey(ciphertext, p.fallbackKey)
	}

	return nil, ErrDecryptionFailed
}

func keychainKey(initialize bool) ([]byte, error) {
	slog.Debug("Attempting to find existing key in macOS Keychain")

	cmd := exec.Command("security", "find-generic-password", "-a", keychainAccount, "-s", keychainService, "-w")
	out, err := cmd.Output()
	if err == nil {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
		if err == nil && len(key) == 32 {
			slog.Debug("Found existing key in macOS Keychain")
			return key, nil
		}
		return nil, fmt.Errorf("existing Knot Keychain item is invalid")
	}
	if !initialize {
		return nil, fmt.Errorf("existing Knot Keychain item unavailable: %w", err)
	}
	// security exit code 44 means item not found; locked/unavailable stores
	// must not be repaired by replacing their key.
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 44 {
		return nil, err
	}

	slog.Debug("Creating a new key in Keychain")
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	keyStr := base64.StdEncoding.EncodeToString(key)

	addCmd := exec.Command("security", "add-generic-password", "-a", keychainAccount, "-s", keychainService, "-w", "-")
	addCmd.Stdin = strings.NewReader(keyStr)
	if err := addCmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to add key to keychain: %w", err)
	}

	slog.Debug("New key created and stored in macOS Keychain successfully")
	return key, nil
}

func getMachineID() (string, error) {
	cmd := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	re := regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`)
	match := re.FindStringSubmatch(string(out))
	if len(match) > 1 {
		return match[1], nil
	}

	return "", fmt.Errorf("IOPlatformUUID not found in ioreg output")
}

//go:build darwin

package crypto

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"knot-core/internal/paths"
)

const (
	keychainAccount = "knot-master-key"
	keychainService = "knot"
	keychainTimeout = 4 * time.Second
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
	key, keychainErr := keychainKey(initialize)

	// 3. Validation
	if key == nil && fallbackKey == nil {
		return nil, fmt.Errorf("macOS crypto provider unavailable: %w", keychainErr)
	}
	if key == nil {
		slog.Warn("macOS Keychain unavailable, using the machine ID fallback", "error", keychainErr)
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
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()
	key, err := readKeychainKey(ctx)
	if err == nil {
		return key, nil
	}
	// Only create an item when Keychain reports that the item is absent.
	const itemNotFound, duplicateItem = 44, 45
	var exitErr *exec.ExitError
	if !initialize || !errors.As(err, &exitErr) || exitErr.ExitCode() != itemNotFound {
		return nil, err
	}

	slog.Debug("Creating a new key in Keychain")
	key = make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	// Pass the complete command over stdin to keep the key out of process arguments.
	addCmd := exec.CommandContext(ctx, "/usr/bin/security", "-i", "-q")
	addCmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -a %s -s %s -w %s\n",
		keychainAccount, keychainService, base64.StdEncoding.EncodeToString(key)))
	if err := addCmd.Run(); err != nil && (!errors.As(err, &exitErr) || exitErr.ExitCode() != duplicateItem) {
		return nil, fmt.Errorf("add Knot Keychain item: %w", errors.Join(ctx.Err(), err))
	}
	// Another Core using a different config may have created the shared item first.
	return readKeychainKey(ctx)
}

func readKeychainKey(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-a", keychainAccount, "-s", keychainService, "-w")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read Knot Keychain item: %w", errors.Join(ctx.Err(), err))
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("existing Knot Keychain item is invalid")
	}
	return key, nil
}

func getMachineID() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
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

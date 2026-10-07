//go:build windows

package crypto

import (
	"fmt"
	"log/slog"
	"unsafe"

	"knot-core/internal/paths"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

type windowsProvider struct {
	fallbackKey []byte
}

func openPlatformProvider(layout paths.Layout, initialize bool) (Provider, error) {
	slog.Debug("Initializing Windows crypto provider")

	machineID, err := getMachineID()
	if err != nil {
		slog.Debug("Failed to get MachineGuid from registry, using hostname as weak ID fallback", "error", err)
		// Last resort: hostname
		machineID, _ = windows.ComputerName()
	}

	salt, err := readSalt(layout, allowSaltInitialization(layout, initialize))
	if err != nil {
		return nil, fmt.Errorf("failed to get salt: %w", err)
	}

	var fallbackKey []byte
	if machineID != "" {
		fallbackKey = DeriveKey(machineID, salt)
	}

	return &windowsProvider{
		fallbackKey: fallbackKey,
	}, nil
}

func (p *windowsProvider) Available() bool { return p.Name() != "None" }
func (p *windowsProvider) Limitations() []string {
	return []string{"DPAPI is bound to the current Windows account; legacy machine fallback is supported"}
}

func (p *windowsProvider) Name() string {
	// Try a simple DPAPI encryption to see if it's healthy
	testData := []byte("health-check")
	var dataIn windows.DataBlob
	dataIn.Size = uint32(len(testData))
	dataIn.Data = &testData[0]

	var dataOut windows.DataBlob
	err := windows.CryptProtectData(&dataIn, nil, nil, 0, nil, 1, &dataOut)
	if err == nil {
		windows.LocalFree(windows.Handle(unsafe.Pointer(dataOut.Data)))
		return "Windows DPAPI"
	}

	if p.fallbackKey != nil {
		return "Machine ID Fallback"
	}

	return "None"
}

// Encrypt encrypts data using Windows DPAPI, falls back to Machine ID if DPAPI fails.
func (p *windowsProvider) Encrypt(plaintext []byte) ([]byte, error) {
	slog.Debug("Attempting encryption using Windows DPAPI")

	if len(plaintext) > 0 {
		var dataIn windows.DataBlob
		dataIn.Size = uint32(len(plaintext))
		dataIn.Data = &plaintext[0]

		var dataOut windows.DataBlob
		// CRYPTPROTECT_UI_FORBIDDEN = 0x1
		err := windows.CryptProtectData(&dataIn, nil, nil, 0, nil, 1, &dataOut)
		if err == nil {
			defer windows.LocalFree(windows.Handle(unsafe.Pointer(dataOut.Data)))
			out := make([]byte, dataOut.Size)
			copy(out, unsafe.Slice(dataOut.Data, dataOut.Size))
			slog.Debug("Data encrypted successfully using DPAPI")
			return out, nil
		}
		slog.Debug("DPAPI encryption failed, falling back to Machine ID", "error", err)
	} else if len(plaintext) == 0 {
		return nil, nil
	}

	if p.fallbackKey == nil {
		return nil, fmt.Errorf("no encryption key available (DPAPI failed and Machine ID not found)")
	}

	return EncryptWithKey(plaintext, p.fallbackKey)
}

// Decrypt decrypts data using Windows DPAPI, falls back to Machine ID if DPAPI fails.
func (p *windowsProvider) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) > 0 {
		slog.Debug("Attempting decryption using Windows DPAPI")
		var dataIn windows.DataBlob
		dataIn.Size = uint32(len(ciphertext))
		dataIn.Data = &ciphertext[0]

		var dataOut windows.DataBlob
		err := windows.CryptUnprotectData(&dataIn, nil, nil, 0, nil, 1, &dataOut)
		if err == nil {
			defer windows.LocalFree(windows.Handle(unsafe.Pointer(dataOut.Data)))
			out := make([]byte, dataOut.Size)
			copy(out, unsafe.Slice(dataOut.Data, dataOut.Size))
			slog.Debug("Data decrypted successfully using DPAPI")
			return out, nil
		}
		slog.Debug("DPAPI decryption failed, trying Machine ID fallback", "error", err)
	} else if len(ciphertext) == 0 {
		return nil, nil
	}

	if p.fallbackKey == nil {
		return nil, ErrDecryptionFailed
	}

	return DecryptWithKey(ciphertext, p.fallbackKey)
}

func getMachineID() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", err
	}
	defer k.Close()

	s, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return "", err
	}
	return s, nil
}

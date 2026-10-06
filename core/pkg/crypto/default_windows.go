//go:build windows

package crypto

import (
	"encoding/base64"
	"fmt"
	"unsafe"

	"knot-core/internal/paths"

	"golang.org/x/sys/windows"
)

type windowsDPAPIProvider struct{}

func providerForState(layout paths.Layout, providerID string) (Provider, error) {
	switch providerID {
	case ProviderWindowsDPAPI:
		return windowsDPAPIProvider{}, nil
	case ProviderLocal:
		return NewLocalProvider(localKeyPath(layout))
	default:
		return nil, fmt.Errorf("unknown crypto provider %q", providerID)
	}
}

func selectDefaultProvider(layout paths.Layout) (Provider, error) {
	return windowsDPAPIProvider{}, nil
}

func (windowsDPAPIProvider) Name() string {
	return ProviderWindowsDPAPI
}

func (windowsDPAPIProvider) Available() bool {
	return true
}

func (windowsDPAPIProvider) Limitations() []string {
	return nil
}

func (windowsDPAPIProvider) Encrypt(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{Size: uint32(len(plaintext)), Data: &plaintext[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, 1, &out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncryptionFailed, err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	result := make([]byte, out.Size)
	copy(result, unsafe.Slice(out.Data, out.Size))
	return []byte(base64.StdEncoding.EncodeToString(result)), nil
}

func (windowsDPAPIProvider) Decrypt(ciphertext []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(string(ciphertext))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecryptionFailed, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{Size: uint32(len(raw)), Data: &raw[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 1, &out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecryptionFailed, err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	result := make([]byte, out.Size)
	copy(result, unsafe.Slice(out.Data, out.Size))
	return result, nil
}

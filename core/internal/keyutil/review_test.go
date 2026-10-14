package keyutil

import (
	"encoding/pem"
	"errors"
	"testing"
)

func TestEncryptedPKCS8HasExplicitUnsupportedReason(t *testing.T) {
	raw := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte{48, 0}})
	for _, password := range []string{"", "provided"} {
		if _, err := Signer(raw, password); !errors.Is(err, ErrUnsupportedPrivateKey) {
			t.Fatal(err)
		}
		if _, err := Inspect(raw, password); !errors.Is(err, ErrUnsupportedPrivateKey) {
			t.Fatal(err)
		}
	}
}

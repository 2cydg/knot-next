package config

import (
	"errors"
	"fmt"
	"os"

	"knot-core/internal/keyutil"
)

func (s *Service) inspectKey(key *KeyMetadata) error {
	raw, err := os.ReadFile(key.SourcePath)
	if err != nil {
		return fmt.Errorf("%w: private key source cannot be read", ErrValidation)
	}
	key.Type, key.Length, key.Fingerprint = "", 0, ""
	return inspectStoredKey(key, raw, "")
}

// Stored/path-backed keys may remain locked until an authentication attempt.
// PEM has no public envelope; retain historical type/length as unverified hints.
func inspectStoredKey(key *KeyMetadata, raw []byte, passphrase string) error {
	meta, err := keyutil.Inspect(raw, passphrase)
	if errors.Is(err, keyutil.ErrPassphraseRequired) && passphrase == "" {
		key.Encrypted = true
		key.Fingerprint = ""
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrValidation, err)
	}
	key.Type = meta.Type
	key.Length = meta.Bits
	key.Fingerprint = meta.Fingerprint
	key.Encrypted = meta.Encrypted
	return nil
}

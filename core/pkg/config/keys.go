package config

import (
	"errors"
	"fmt"

	"knot-core/internal/keyutil"
)

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

const maxKeyBytes = keyutil.MaxSourceBytes

func readPrivateKeySource(sourcePath string) ([]byte, error) {
	raw, err := keyutil.ReadSource(sourcePath)
	if err != nil {
		return nil, fieldFailure("keys", "source_path", err.Error())
	}
	return raw, nil
}

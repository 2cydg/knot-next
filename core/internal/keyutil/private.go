package keyutil

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

var ErrPassphraseRequired = errors.New("private key passphrase required")
var ErrInvalidPrivateKey = errors.New("invalid private key or passphrase")

type Metadata struct {
	Type        string
	Bits        int
	Fingerprint string
	Encrypted   bool
}

// Inspect can derive metadata from a locked OpenSSH public envelope. A locked
// PEM has no public envelope: its metadata remains unknown until unlocked.
func Inspect(raw []byte, passphrase string) (Metadata, error) {
	signer, encrypted, err := parseSigner(raw, passphrase)
	var pub ssh.PublicKey
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if passphrase == "" && errors.As(err, &missing) {
			if missing.PublicKey == nil {
				if !validLockedPEM(raw) {
					return Metadata{}, ErrInvalidPrivateKey
				}
				return Metadata{Encrypted: true}, ErrPassphraseRequired
			}
			pub = missing.PublicKey
			encrypted = true
		} else {
			return Metadata{}, ErrInvalidPrivateKey
		}
	} else {
		pub = signer.PublicKey()
	}
	return publicMetadata(pub, encrypted)
}

// ssh checks Proc-Type before the block type or encryption headers. Check the
// envelope before accepting an opaque PEM for deferred validation.
func validLockedPEM(raw []byte) bool {
	block, _ := pem.Decode(raw)
	if block == nil || (block.Type != "RSA PRIVATE KEY" && block.Type != "EC PRIVATE KEY") || !x509.IsEncryptedPEMBlock(block) {
		return false
	}
	cipher, ivHex, ok := strings.Cut(block.Headers["DEK-Info"], ",")
	if !ok {
		return false
	}
	blockSize := 0
	switch cipher {
	case "AES-128-CBC", "AES-192-CBC", "AES-256-CBC":
		blockSize = 16
	case "DES-CBC", "DES-EDE3-CBC":
		blockSize = 8
	default:
		return false
	}
	iv, err := hex.DecodeString(ivHex)
	return err == nil && len(iv) == blockSize && len(block.Bytes) > 0 && len(block.Bytes)%blockSize == 0
}

// An unnecessary passphrase is harmless for a key that parses without one.
func parseSigner(raw []byte, passphrase string) (ssh.Signer, bool, error) {
	signer, err := ssh.ParsePrivateKey(raw)
	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		return signer, false, err
	}
	if passphrase == "" {
		return nil, true, err
	}
	signer, err = ssh.ParsePrivateKeyWithPassphrase(raw, []byte(passphrase))
	return signer, true, err
}
func publicMetadata(pub ssh.PublicKey, encrypted bool) (Metadata, error) {
	meta := Metadata{Fingerprint: ssh.FingerprintSHA256(pub), Encrypted: encrypted}
	public, ok := pub.(ssh.CryptoPublicKey)
	if !ok {
		return meta, ErrInvalidPrivateKey
	}
	switch key := public.CryptoPublicKey().(type) {
	case *rsa.PublicKey:
		meta.Type = "rsa"
		meta.Bits = key.N.BitLen()
	case ed25519.PublicKey:
		meta.Type = "ed25519"
		meta.Bits = 256
	case *ecdsa.PublicKey:
		meta.Type = "ecdsa"
		meta.Bits = key.Curve.Params().BitSize
	default:
		return meta, ErrInvalidPrivateKey
	}
	return meta, nil
}
func Signer(raw []byte, passphrase string) (ssh.Signer, error) {
	signer, _, err := parseSigner(raw, passphrase)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		return nil, ErrPassphraseRequired
	}
	if err != nil {
		return nil, ErrInvalidPrivateKey
	}
	return signer, nil
}

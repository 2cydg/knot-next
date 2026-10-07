package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"knot-core/internal/fileutil"
	"knot-core/internal/paths"

	"golang.org/x/crypto/pbkdf2"
)

var (
	ErrEncryptionFailed = errors.New("encryption failed")
	ErrDecryptionFailed = errors.New("decryption failed")
)

type Provider interface {
	Name() string
	Available() bool
	Limitations() []string
	Encrypt([]byte) ([]byte, error)
	Decrypt([]byte) ([]byte, error)
}

type Capability struct {
	Provider    string   `json:"provider"`
	Available   bool     `json:"available"`
	Limitations []string `json:"limitations,omitempty"`
}

const (
	ProviderLocal          = "local-aes-gcm"
	ProviderLinuxSecret    = "linux-secret-service"
	ProviderLinuxMachine   = "linux-machine-id"
	ProviderWindowsDPAPI   = "windows-dpapi"
	ProviderDarwinKeychain = "darwin-keychain"

	saltFile           = ".salt"
	saltLength         = 32
	pbkdf2Iterations   = 100000
	cryptoStateFile    = ".crypto-state"
	cryptoStateVersion = 1
	probePlainLength   = 32
)

func CapabilityOf(provider Provider) Capability {
	if provider == nil {
		return Capability{
			Provider:    "none",
			Available:   false,
			Limitations: []string{"secret encryption provider is not configured"},
		}
	}
	return Capability{
		Provider:    provider.Name(),
		Available:   provider.Available(),
		Limitations: append([]string(nil), provider.Limitations()...),
	}
}

type LocalProvider struct {
	key []byte
}

func NewLocalProvider(keyPath string) (*LocalProvider, error) {
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(key)
	return &LocalProvider{key: sum[:]}, nil
}

func NewStaticProvider(key []byte) *LocalProvider {
	sum := sha256.Sum256(key)
	return &LocalProvider{key: sum[:]}
}

func (p *LocalProvider) Name() string {
	return ProviderLocal
}

func (p *LocalProvider) Available() bool {
	return len(p.key) == 32
}

func (p *LocalProvider) Limitations() []string {
	return []string{"local key file protects secrets; platform credential store integration is not active"}
}

func (p *LocalProvider) Encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(p.key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncryptionFailed, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncryptionFailed, err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncryptionFailed, err)
	}
	out := make([]byte, 0, len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plaintext, nil)
	return out, nil
}

func (p *LocalProvider) Decrypt(ciphertext []byte) ([]byte, error) {
	raw := ciphertext
	block, err := aes.NewCipher(p.key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecryptionFailed, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecryptionFailed, err)
	}
	if len(raw) < gcm.NonceSize() {
		return nil, ErrDecryptionFailed
	}
	nonce := raw[:gcm.NonceSize()]
	body := raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecryptionFailed, err)
	}
	return plaintext, nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("crypto key path is required")
	}
	if key, err := os.ReadFile(path); err == nil {
		if len(key) == 0 {
			return nil, errors.New("crypto key file is empty")
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ReadFile(path)
		}
		return nil, err
	}
	if _, err := file.Write(key); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return key, nil
}

type KeyProvider struct {
	name        string
	key         []byte
	limitations []string
}

func NewKeyProvider(name string, key []byte, limitations []string) *KeyProvider {
	return &KeyProvider{name: name, key: append([]byte(nil), key...), limitations: append([]string(nil), limitations...)}
}

func (p *KeyProvider) Name() string {
	return p.name
}

func (p *KeyProvider) Available() bool {
	return len(p.key) == 32
}

func (p *KeyProvider) Limitations() []string {
	return append([]string(nil), p.limitations...)
}

func (p *KeyProvider) Encrypt(plaintext []byte) ([]byte, error) {
	return encryptWithKey(plaintext, p.key)
}

func (p *KeyProvider) Decrypt(ciphertext []byte) ([]byte, error) {
	return decryptWithKey(ciphertext, p.key)
}

func DeriveKey(material string, salt []byte) []byte {
	return pbkdf2.Key([]byte(material), salt, pbkdf2Iterations, 32, sha256.New)
}

func GetSalt(layout paths.Layout) ([]byte, error) {
	configDir := layout.ConfigDir
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return nil, err
	}

	saltPath := filepath.Join(configDir, saltFile)

	salt := make([]byte, saltLength)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}

	if err := createSaltFile(saltPath, salt); err == nil {
		return salt, nil
	} else if !os.IsExist(err) {
		return nil, err
	}

	existing, err := os.ReadFile(saltPath)
	if err != nil {
		return nil, err
	}
	if len(existing) != saltLength {
		return nil, fmt.Errorf("invalid salt length: %d", len(existing))
	}
	return existing, nil
}

func createSaltFile(saltPath string, salt []byte) error {
	f, err := os.CreateTemp(filepath.Dir(saltPath), saltFile+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	if _, err := f.Write(salt); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	return os.Link(tmpPath, saltPath)
}

type State struct {
	Version         int    `json:"version"`
	Provider        string `json:"provider"`
	ProbeCiphertext string `json:"probe_ciphertext"`
	ProbeHash       string `json:"probe_hash"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func LoadState(layout paths.Layout) (*State, error) {
	path := filepath.Join(layout.ConfigDir, cryptoStateFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("invalid crypto state: %w", err)
	}
	if state.Version != cryptoStateVersion {
		return nil, fmt.Errorf("invalid crypto state version: %d", state.Version)
	}
	if state.Provider == "" || state.ProbeCiphertext == "" || state.ProbeHash == "" {
		return nil, errors.New("invalid crypto state: missing required fields")
	}
	if _, err := hex.DecodeString(state.ProbeHash); err != nil {
		return nil, fmt.Errorf("invalid crypto state probe hash: %w", err)
	}
	return &state, nil
}

func PersistState(layout paths.Layout, provider Provider) error {
	state, err := NewState(provider)
	if err != nil {
		return err
	}
	raw, err := MarshalState(state)
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(layout.ConfigDir, cryptoStateFile), raw, 0o600)
}

// MarshalState preserves the legacy state file's indentation and final newline.
// Bootstrap stages these bytes in its transaction instead of persisting early.
func MarshalState(state *State) ([]byte, error) {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func NewState(provider Provider) (*State, error) {
	probe := make([]byte, probePlainLength)
	if _, err := io.ReadFull(rand.Reader, probe); err != nil {
		return nil, err
	}
	ciphertext, err := provider.Encrypt(probe)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(probe)
	now := time.Now().UTC().Format(time.RFC3339)
	return &State{
		Version:         cryptoStateVersion,
		Provider:        provider.Name(),
		ProbeCiphertext: base64.StdEncoding.EncodeToString(ciphertext),
		ProbeHash:       hex.EncodeToString(sum[:]),
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

func ValidateState(state *State, provider Provider) error {
	if state.Provider != provider.Name() {
		return fmt.Errorf("crypto provider state is %q, got %q", state.Provider, provider.Name())
	}
	raw, err := base64.StdEncoding.DecodeString(state.ProbeCiphertext)
	if err != nil {
		return ErrDecryptionFailed
	}
	plaintext, err := provider.Decrypt(raw)
	if err != nil {
		return fmt.Errorf("crypto state probe decrypt failed: %w", err)
	}
	sum := sha256.Sum256(plaintext)
	if hex.EncodeToString(sum[:]) != state.ProbeHash {
		return errors.New("crypto state probe hash mismatch")
	}
	return nil
}

// NewDefaultProvider is the explicit initializing path used by daemon startup.
func NewDefaultProvider(layout paths.Layout) (Provider, error) {
	return openPlatformProvider(layout, true)
}

// OpenExistingProvider never creates or repairs salt, state, or platform keys.
// Migration preview and inspection must use this path.
func OpenExistingProvider(layout paths.Layout) (Provider, error) {
	return openPlatformProvider(layout, false)
}

func readSalt(layout paths.Layout, initialize bool) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(layout.ConfigDir, saltFile))
	if err == nil {
		if len(raw) != saltLength {
			return nil, fmt.Errorf("invalid crypto salt length")
		}
		return raw, nil
	}
	if !errors.Is(err, os.ErrNotExist) || !initialize {
		return nil, err
	}
	return GetSalt(layout)
}

func localKeyPath(layout paths.Layout) string {
	return filepath.Join(layout.ConfigDir, "secret.key")
}

func encryptWithKey(plaintext []byte, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncryptionFailed, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncryptionFailed, err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncryptionFailed, err)
	}
	out := make([]byte, 0, len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plaintext, nil)
	return out, nil
}

func decryptWithKey(ciphertext []byte, key []byte) ([]byte, error) {
	raw := ciphertext
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecryptionFailed, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecryptionFailed, err)
	}
	if len(raw) < gcm.NonceSize() {
		return nil, ErrDecryptionFailed
	}
	nonce := raw[:gcm.NonceSize()]
	body := raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecryptionFailed, err)
	}
	return plaintext, nil
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return fileutil.AtomicWriteFile(path, data, perm)
}

// Legacy provider identifiers and primitives retain the original raw-byte contract.
const (
	ProviderLinuxSecretService = ProviderLinuxSecret
	ProviderLinuxMachineID     = ProviderLinuxMachine
)

func EncryptWithKey(plaintext, key []byte) ([]byte, error)  { return encryptWithKey(plaintext, key) }
func DecryptWithKey(ciphertext, key []byte) ([]byte, error) { return decryptWithKey(ciphertext, key) }

func allowSaltInitialization(layout paths.Layout, initialize bool) bool {
	if !initialize {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(layout.ConfigDir, "config.toml"))
	return errors.Is(err, os.ErrNotExist) || err == nil && !bytes.Contains(raw, []byte("ENC:"))
}

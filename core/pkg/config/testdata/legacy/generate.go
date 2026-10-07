//go:build ignore

// Run from the old knot module: go run ../knot-next/core/pkg/config/testdata/legacy/generate.go OUTPUT_DIR
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"knot/pkg/config"
	"knot/pkg/crypto"
)

type fixed struct {
	name string
	key  []byte
}

func (p fixed) Name() string                     { return p.name }
func (p fixed) Encrypt(v []byte) ([]byte, error) { return crypto.EncryptWithKey(v, p.key) }
func (p fixed) Decrypt(v []byte) ([]byte, error) { return crypto.DecryptWithKey(v, p.key) }
func must(err error) {
	if err != nil {
		panic(err)
	}
}
func main() {
	root := os.Args[1]
	seed := sha256.Sum256([]byte("Knot legacy fixture artificial private key"))
	key := ed25519.NewKeyFromSeed(seed[:])
	block, err := ssh.MarshalPrivateKey(key, "artificial fixture")
	must(err)
	private := string(pem.EncodeToMemory(block))
	b := func(v bool) *bool { return &v }
	cfg := config.Config{
		Settings: config.SettingsConfig{ForwardAgent: b(false), ClearScreenOnConnect: b(false), BroadcastEscapeEnable: b(true), BroadcastEscapeChar: "!", IdleTimeout: "15m", KeepaliveInterval: "10s", LogLevel: "info", RecentLimit: 7, DefaultSFTPLocalPath: "~/fixture files", DefaultSyncProvider: "cloud", SyncPassword: "fixture-sync-password"},
		Servers: map[string]config.ServerConfig{
			"srv_jump":   {ID: "srv_jump", Alias: "jump", Host: "jump.example", Port: 2222, User: "fixture", AuthMethod: "agent"},
			"srv_legacy": {ID: "srv_legacy", Alias: "prod", Host: "prod.example", Port: 2222, User: "fixture", AuthMethod: "key", Password: "fixture-server-password", KeyID: "key_legacy", ProxyID: "prx_legacy", JumpHostIDs: []string{"srv_jump"}, Tags: []string{"测试", "fixture"}, Forwards: []config.ForwardConfig{{Type: "L", LocalPort: 8080, RemoteAddr: "127.0.0.1:80"}, {Type: "D", LocalPort: 1080}}},
		},
		Proxies: map[string]config.ProxyConfig{"prx_legacy": {ID: "prx_legacy", Alias: "proxy", Type: "socks5", Host: "proxy.example", Port: 1080, Username: "fixture", Password: "fixture-proxy-password"}},
		Keys:    map[string]config.KeyConfig{"key_legacy": {ID: "key_legacy", Alias: "deploy", Type: "ed25519", Length: 256, PrivateKey: private}},
		SyncProviders: map[string]config.SyncProviderConfig{
			"sync_web": {ID: "sync_web", Alias: "cloud", Type: "webdav", URL: "https://dav.example.test", Username: "fixture", Password: "fixture-webdav-password"},
			"sync_s3":  {ID: "sync_s3", Alias: "object", Type: "s3", Bucket: "fixture", Key: "config.toml", Region: "test", AccessKeyID: "fixture-access", SecretAccessKey: "fixture-secret", SessionToken: "fixture-token", PathStyle: true},
		},
	}
	salt := make([]byte, 32)
	for i := range salt {
		salt[i] = byte(i)
	}
	rawKey := make([]byte, 32)
	for i := range rawKey {
		rawKey[i] = byte(255 - i)
	}
	providers := []fixed{{crypto.ProviderLinuxSecretService, rawKey}, {crypto.ProviderLinuxMachineID, crypto.DeriveKey("fixture-machine\x001000", salt)}}
	hashes := map[string]string{}
	for _, p := range providers {
		dir := filepath.Join(root, p.name)
		must(os.MkdirAll(dir, 0700))
		must(cfg.SaveToPath(filepath.Join(dir, "config.toml"), p))
		state, err := crypto.NewState(p)
		must(err)
		state.CreatedAt = "2026-10-07T00:00:00Z"
		state.UpdatedAt = state.CreatedAt
		data, err := json.MarshalIndent(state, "", "  ")
		must(err)
		must(os.WriteFile(filepath.Join(dir, ".crypto-state"), data, 0600))
		must(os.WriteFile(filepath.Join(dir, ".salt"), salt, 0600))
		signer, err := ssh.NewSignerFromKey(key)
		must(err)
		hosts := knownhosts.Line([]string{knownhosts.HashHostname("[prod.example]:2222")}, signer.PublicKey()) + "\n"
		must(os.WriteFile(filepath.Join(dir, "known_hosts"), []byte(hosts), 0600))
		recent := config.State{Recent: []config.RecentEntry{{ServerID: "srv_legacy", LastUsed: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}, {ServerID: "srv_deleted", LastUsed: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}}}
		data, err = json.MarshalIndent(recent, "", "  ")
		must(err)
		must(os.WriteFile(filepath.Join(dir, "state.json"), data, 0600))
	}
	for name, v := range map[string]string{"server_password": "fixture-server-password", "proxy_password": "fixture-proxy-password", "sync_password": "fixture-sync-password", "webdav_password": "fixture-webdav-password", "access_key_id": "fixture-access", "secret_access_key": "fixture-secret", "session_token": "fixture-token", "private_key": private} {
		sum := sha256.Sum256([]byte(v))
		hashes[name] = hex.EncodeToString(sum[:])
	}
	data, err := json.MarshalIndent(hashes, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(root, "plaintext-hashes.json"), data, 0600))
	fmt.Println("Generated fixed legacy fixtures with the old Knot implementation; regenerate intentionally, ciphertext is random.")
}

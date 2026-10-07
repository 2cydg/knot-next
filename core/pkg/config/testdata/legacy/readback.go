//go:build ignore

// Run from the old knot module to check a new core writeback with the old loader.
package main

import (
	"bytes"
	"fmt"
	"github.com/BurntSushi/toml"
	"knot/pkg/config"
	"knot/pkg/crypto"
	"os"
)

type fixtureProvider struct{ key []byte }

func (p fixtureProvider) Name() string                     { return crypto.ProviderLinuxSecretService }
func (p fixtureProvider) Encrypt(v []byte) ([]byte, error) { return crypto.EncryptWithKey(v, p.key) }
func (p fixtureProvider) Decrypt(v []byte) ([]byte, error) { return crypto.DecryptWithKey(v, p.key) }
func main() {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(255 - i)
	}
	cfg, err := config.LoadFromPath(os.Args[1], fixtureProvider{key})
	if err != nil {
		panic(err)
	}
	if cfg.Servers["srv_legacy"].Password != "fixture-server-password" || cfg.Proxies["prx_legacy"].Password != "fixture-proxy-password" || cfg.Settings.DefaultSyncProvider != "cloud" || cfg.Settings.GetClearScreenOnConnect() || !cfg.Settings.GetBroadcastEscapeEnable() || cfg.Settings.RecentLimit != 7 || len(cfg.Servers["srv_legacy"].Forwards) != 2 || cfg.Keys["key_legacy"].Type != "ed25519" || cfg.SyncProviders["sync_s3"].SecretAccessKey != "fixture-secret" {
		panic("legacy readback mismatch")
	}
	if len(os.Args) > 2 {
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
			panic(err)
		}
		if err := os.WriteFile(os.Args[2], buf.Bytes(), 0600); err != nil {
			panic(err)
		}
	}
	fmt.Println("Old Knot loader successfully read TOML, credentials, default alias, forwards, and client preferences.")
}

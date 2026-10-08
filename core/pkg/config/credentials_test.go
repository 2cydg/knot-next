package config

import (
	"knot-core/pkg/crypto"
	"testing"
	"time"
)

func TestRememberAuthPreservesLatestProfileAndSwitchesChoice(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.CreateKey(KeyMetadata{ID: "key", Alias: "key"}); err != nil {
		t.Fatal(err)
	}
	profile := ServerProfile{ID: "server", Alias: "prod", Host: "old-host", Port: 22, User: "user", AuthMethod: AuthMethodAgent}
	if _, err := svc.CreateServer(profile); err != nil {
		t.Fatal(err)
	}
	// The credential candidate was obtained before the user edited this profile.
	profile.Host = "new-host"
	profile.Tags = []string{"edited"}
	if _, err := svc.UpdateServer(profile.ID, profile); err != nil {
		t.Fatal(err)
	}
	for _, choice := range []AuthChoice{{Method: AuthMethodPassword, Password: "verified"}, {Method: AuthMethodKey, KeyID: "key"}, {Method: AuthMethodAgent}} {
		if err := svc.RememberServerAuth(profile.ID, choice); err != nil {
			t.Fatal(err)
		}
		runtime, err := svc.RuntimeConfig()
		if err != nil {
			t.Fatal(err)
		}
		got := runtime.Servers[profile.ID]
		if got.AuthMethod != choice.Method || got.Host != "new-host" || len(got.Tags) != 1 {
			t.Fatalf("credential save rewrote profile: %+v", got)
		}
		if got.Password != choice.Password || got.KeyID != choice.KeyID {
			t.Fatalf("selection mismatch: %+v", got)
		}
	}
}

// Pause encryption inside the Remember transaction, while a second service
// attempts a profile edit through the shared on-disk lock.
type reviewGatedCrypto struct {
	crypto.Provider
	entered chan struct{}
	gate    chan struct{}
}

func (p *reviewGatedCrypto) Encrypt(data []byte) ([]byte, error) {
	close(p.entered)
	<-p.gate
	return p.Provider.Encrypt(data)
}
func TestRememberAuthConcurrentProfileEdit(t *testing.T) {
	svc := newTestService(t)
	profile := ServerProfile{ID: "server", Alias: "server", Host: "old", Port: 22, User: "u", AuthMethod: AuthMethodPassword}
	if _, err := svc.CreateServer(profile); err != nil {
		t.Fatal(err)
	}
	gated := &reviewGatedCrypto{Provider: svc.crypto, entered: make(chan struct{}), gate: make(chan struct{})}
	svc.crypto = gated
	editor := NewService(svc.layout, crypto.NewStaticProvider([]byte("test-key")))
	released := false
	defer func() {
		if !released {
			close(gated.gate)
		}
	}()
	saved := make(chan error, 1)
	go func() {
		saved <- svc.RememberServerAuth("server", AuthChoice{Method: AuthMethodPassword, Password: "verified"})
	}()
	select {
	case <-gated.entered:
	case <-time.After(time.Second):
		t.Fatal("remember did not reach encryption")
	}
	edited := make(chan error, 1)
	started := make(chan struct{})
	profile.Host = "edited"
	profile.Tags = []string{"keep"}
	go func() { close(started); _, err := editor.UpdateServer("server", profile); edited <- err }()
	<-started
	// A transaction must keep the file lock while encrypting and committing.
	select {
	case err := <-edited:
		t.Fatalf("edit crossed unfinished auth transaction: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(gated.gate)
	released = true
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	if err := <-edited; err != nil {
		t.Fatal(err)
	}
	runtime, err := svc.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	got := runtime.Servers["server"]
	if got.Host != "edited" || len(got.Tags) != 1 || got.Password != "verified" || got.AuthMethod != AuthMethodPassword {
		t.Fatalf("concurrent edit/auth lost: %+v", got)
	}
}

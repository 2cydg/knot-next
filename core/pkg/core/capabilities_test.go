package core

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"knot-core/internal/logger"
	"knot-core/internal/paths"
	"knot-core/pkg/config"
	"knot-core/pkg/secret"
)

func TestCapabilityProbesCoalesceExpireAndInvalidate(t *testing.T) {
	svc := New("test", time.Now())
	var calls atomic.Int32
	svc.agentProbe = func(context.Context) (bool, string) { calls.Add(1); return true, "" }
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); svc.Capabilities() }()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("probes=%d", calls.Load())
	}
	svc.probes.mu.Lock()
	svc.probes.expires = time.Now().Add(-time.Second)
	svc.probes.mu.Unlock()
	svc.Capabilities()
	if calls.Load() != 2 {
		t.Fatal("expiry ignored")
	}
	svc.UseSecret(secret.NewService(nil, nil))
	svc.Capabilities()
	if calls.Load() != 3 {
		t.Fatal("replacement ignored")
	}
	svc.UseSecret(svc.Secret())
	svc.Capabilities()
	if calls.Load() != 4 {
		t.Fatal("same dependency refresh ignored")
	}
}

func TestCapabilityCancellationDoesNotPoisonCache(t *testing.T) {
	svc := New("test", time.Now())
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	svc.agentProbe = func(ctx context.Context) (bool, string) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-ctx.Done():
				return false, "canceled"
			case <-release:
			}
		}
		return true, ""
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.CapabilitiesContext(ctx); close(done) }()
	<-started
	waiting, cancelWaiting := context.WithCancel(context.Background())
	waiterDone := make(chan capabilityProbeResult)
	go func() { waiterDone <- svc.capabilityProbes(waiting, nil, nil) }()
	cancelWaiting()
	select {
	case result := <-waiterDone:
		if result.agentReason != "probe_canceled" {
			t.Fatal(result)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting request could not cancel")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked")
	}
	close(release)
	svc.Capabilities()
	if calls.Load() != 2 {
		t.Fatal("canceled result cached")
	}
}

func TestBusinessEventsRespectLevelAndSaturationIsObservable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.log")
	file, err := logger.Open(path, logger.Options{Level: slog.LevelError})
	if err != nil {
		t.Fatal(err)
	}
	old := slog.Default()
	slog.SetDefault(file.Logger())
	defer slog.SetDefault(old)
	svc := New("test", time.Now())
	svc.UseLogger(file)
	svc.agentProbe = func(context.Context) (bool, string) { return false, "agent_unavailable" }
	svc.PublishEvent(Event{Type: "session.connected", Data: map[string]any{"state": "connected"}})
	svc.PublishEvent(Event{Type: "sftp.session.closed", Data: map[string]any{"state": "closed", "error": "client requested close"}})
	svc.PublishEvent(Event{Type: "session.failed", Data: map[string]any{"state": "failed", "error": "connection_refused"}})
	svc.PublishEvent(Event{Type: "core.shutdown_started"})
	file.Redactor().Add(strings.Repeat("private-sentinel", 400000))
	logger.Diagnostic(slog.LevelInfo, "core.ready", "error", "private-sentinel")
	found := false
	for _, c := range svc.Capabilities() {
		if c.Name == "file_log" {
			found = true
			if c.Available == nil || *c.Available || c.Reason != "redaction_saturated" {
				t.Fatalf("missing degradation: %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("missing log capability")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "session.connected") || strings.Contains(text, "client requested close") || strings.Contains(text, "private-sentinel") || !strings.Contains(text, "session.failed") || !strings.Contains(text, "core.shutdown_started") || !strings.Contains(text, `"redaction_status":"saturated"`) {
		t.Fatalf("log behavior: %s", text)
	}
}

type capabilityTestProvider struct {
	available atomic.Bool
	decrypts  atomic.Int32
}

func (p *capabilityTestProvider) Name() string          { return "test" }
func (p *capabilityTestProvider) Available() bool       { return p.available.Load() }
func (p *capabilityTestProvider) Limitations() []string { return nil }
func (p *capabilityTestProvider) Encrypt(data []byte) ([]byte, error) {
	return append([]byte(nil), data...), nil
}
func (p *capabilityTestProvider) Decrypt(data []byte) ([]byte, error) {
	p.decrypts.Add(1)
	if !p.available.Load() {
		return nil, errors.New("provider unavailable")
	}
	return append([]byte(nil), data...), nil
}

func TestCapabilityCryptoHealthAndConfigFailures(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "config"), filepath.Join(root, "state"))
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	provider := &capabilityTestProvider{}
	provider.available.Store(true)
	cfg := config.NewService(layout, provider)
	if _, err := cfg.CreateServer(config.ServerProfile{ID: "target", Alias: "web", Host: "host", Port: 22, User: "user", AuthMethod: config.AuthMethodPassword}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.SetServerPassword("target", "private-value"); err != nil {
		t.Fatal(err)
	}
	svc := New("test", time.Now())
	svc.UseConfig(cfg)
	svc.UseSecret(secret.NewService(cfg, provider))
	svc.agentProbe = func(context.Context) (bool, string) { return false, "agent_unavailable" }
	lookup := func() Capability {
		t.Helper()
		for _, c := range svc.Capabilities() {
			if c.Name == "crypto" {
				return c
			}
		}
		t.Fatal("missing crypto")
		return Capability{}
	}
	if c := lookup(); c.Available == nil || !*c.Available {
		t.Fatalf("healthy provider: %+v", c)
	}
	before := provider.decrypts.Load()
	lookup()
	if provider.decrypts.Load() != before {
		t.Fatal("cached query decrypted config")
	}
	provider.available.Store(false)
	svc.invalidateCapabilityProbes()
	if c := lookup(); c.Available == nil || *c.Available {
		t.Fatalf("provider fault ignored: %+v", c)
	}
	provider.available.Store(true)
	if err := os.WriteFile(filepath.Join(layout.ConfigDir, "config.toml"), []byte("invalid TOML !"), 0600); err != nil {
		t.Fatal(err)
	}
	svc.invalidateCapabilityProbes()
	if c := lookup(); c.Available == nil || *c.Available || c.Reason != "configuration_or_crypto_unavailable" {
		t.Fatalf("config fault ignored: %+v", c)
	}
}

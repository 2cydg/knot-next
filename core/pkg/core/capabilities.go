package core

import (
	"context"
	"sync"
	"time"

	"knot-core/pkg/config"
	"knot-core/pkg/secret"
)

const capabilityProbeTTL = time.Second

type capabilityProbeResult struct {
	agentAvailable, cryptoAvailable bool
	agentReason, cryptoReason       string
	cryptoLimits                    []string
}
type capabilityProbeCache struct {
	mu         sync.Mutex
	inflight   chan struct{}
	expires    time.Time
	config     *config.Service
	secret     *secret.Service
	result     capabilityProbeResult
	generation uint64
}

func (s *Service) invalidateCapabilityProbes() {
	s.probes.mu.Lock()
	s.probes.expires = time.Time{}
	s.probes.generation++
	s.probes.mu.Unlock()
}

// Only costly probes are cached. Resource presence and log degradation are
// sampled live. I/O never holds the dependency lock; waiting callers can cancel.
func (s *Service) capabilityProbes(ctx context.Context, cfg *config.Service, secrets *secret.Service) capabilityProbeResult {
	c := &s.probes
	for {
		if ctx.Err() != nil {
			return capabilityProbeResult{agentReason: "probe_canceled", cryptoReason: "probe_canceled"}
		}
		c.mu.Lock()
		if c.config == cfg && c.secret == secrets && time.Now().Before(c.expires) {
			result := c.result
			c.mu.Unlock()
			return result
		}
		if done := c.inflight; done != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
			case <-done:
			}
			continue
		}
		done := make(chan struct{})
		generation := c.generation
		c.inflight = done
		c.mu.Unlock()

		result := capabilityProbeResult{cryptoReason: "provider_unavailable"}
		result.agentAvailable, result.agentReason = s.agentProbe(ctx)
		if ctx.Err() == nil && secrets != nil {
			crypto := secrets.CryptoCapability()
			result.cryptoAvailable, result.cryptoReason = crypto.Available, crypto.Provider
			result.cryptoLimits = append([]string(nil), crypto.Limitations...)
			if crypto.Available && cfg != nil && ctx.Err() == nil {
				if _, err := cfg.RuntimeConfig(); err != nil {
					result.cryptoAvailable = false
					result.cryptoReason = "configuration_or_crypto_unavailable"
				}
			}
		}
		c.mu.Lock()
		if ctx.Err() == nil && c.generation == generation {
			c.result, c.config, c.secret = result, cfg, secrets
			c.expires = time.Now().Add(capabilityProbeTTL)
		}
		c.inflight = nil
		close(done)
		c.mu.Unlock()
		if ctx.Err() != nil {
			return capabilityProbeResult{agentReason: "probe_canceled", cryptoReason: "probe_canceled"}
		}
		return result
	}
}

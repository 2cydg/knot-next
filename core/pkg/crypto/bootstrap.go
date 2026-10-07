package crypto

import (
	"sync"

	"knot-core/internal/paths"
)

type BootstrapProvider struct {
	layout       paths.Layout
	mu           sync.Mutex
	selected     Provider
	candidates   []Provider
	afterPersist func() (Provider, error)
	persisted    Provider
	reason       string
}

func NewBootstrapProvider(selected Provider, candidates []Provider, afterPersist func() (Provider, error)) Provider {
	return &BootstrapProvider{selected: selected, candidates: candidates, afterPersist: afterPersist}
}

func NewBootstrapProviderWithReason(selected Provider, candidates []Provider, afterPersist func() (Provider, error), reason string) Provider {
	return &BootstrapProvider{selected: selected, candidates: candidates, afterPersist: afterPersist, reason: reason}
}

func (p *BootstrapProvider) Encrypt(plaintext []byte) ([]byte, error) {
	return p.selected.Encrypt(plaintext)
}

func (p *BootstrapProvider) Decrypt(ciphertext []byte) ([]byte, error) {
	return p.selected.Decrypt(ciphertext)
}

func (p *BootstrapProvider) Name() string {
	return p.selected.Name()
}

func (p *BootstrapProvider) Selected() Provider {
	return p.selected
}

func (p *BootstrapProvider) Candidates() []Provider {
	out := make([]Provider, len(p.candidates))
	copy(out, p.candidates)
	return out
}

func (p *BootstrapProvider) AfterPersist() (Provider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.persisted != nil {
		return p.persisted, nil
	}
	if p.afterPersist == nil {
		p.persisted = p.selected
		return p.persisted, nil
	}
	provider, err := p.afterPersist()
	if err != nil {
		return nil, err
	}
	p.persisted = provider
	return p.persisted, nil
}

func (p *BootstrapProvider) MarkPersisted(provider Provider) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.persisted = provider
}

func (p *BootstrapProvider) Persisted() Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.persisted
}

func (p *BootstrapProvider) Reason() string {
	return p.reason
}

func IsBootstrapProvider(provider Provider) (*BootstrapProvider, bool) {
	p, ok := provider.(*BootstrapProvider)
	return p, ok
}

func UnwrapBootstrapProvider(provider Provider) Provider {
	if p, ok := IsBootstrapProvider(provider); ok {
		return p.Selected()
	}
	return provider
}

func (p *BootstrapProvider) Available() bool { return p.selected.Available() }
func (p *BootstrapProvider) Limitations() []string {
	out := append([]string(nil), p.selected.Limitations()...)
	if p.reason != "" {
		out = append(out, "preferred platform credential store unavailable; using legacy fallback")
	}
	return out
}
func (p *BootstrapProvider) Layout() paths.Layout { return p.layout }

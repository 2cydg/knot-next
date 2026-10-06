//go:build !linux && !windows && !darwin

package crypto

import (
	"fmt"

	"knot-core/internal/paths"
)

func providerForState(layout paths.Layout, providerID string) (Provider, error) {
	switch providerID {
	case ProviderLocal:
		return NewLocalProvider(localKeyPath(layout))
	default:
		return nil, fmt.Errorf("unknown crypto provider %q", providerID)
	}
}

func selectDefaultProvider(layout paths.Layout) (Provider, error) {
	return NewLocalProvider(localKeyPath(layout))
}

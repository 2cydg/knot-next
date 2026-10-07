//go:build !linux && !darwin && !windows

package crypto

import (
	"fmt"

	"knot-core/internal/paths"
)

func openPlatformProvider(layout paths.Layout, initialize bool) (Provider, error) {
	return nil, fmt.Errorf("unsupported crypto platform")
}

//go:build !windows && !darwin

package i18n

import (
	"context"
	"os"
)

type SystemProvider struct{}

func (SystemProvider) PreferredLanguages(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return localePreferences(os.Getenv), nil
}

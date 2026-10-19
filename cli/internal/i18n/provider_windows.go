package i18n

import (
	"context"

	"golang.org/x/sys/windows"
)

type SystemProvider struct{}

func (SystemProvider) PreferredLanguages(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return windows.GetUserPreferredUILanguages(windows.MUI_LANGUAGE_NAME)
}

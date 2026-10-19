package i18n

import (
	"context"
	"os"
	"os/exec"
	"time"
)

type SystemProvider struct{}

func (SystemProvider) PreferredLanguages(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	queryCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	// Absolute system binary, fixed arguments, no shell and no CGO dependency.
	cmd := exec.CommandContext(queryCtx, "/usr/bin/defaults", "read", "-g", "AppleLanguages")
	cmd.WaitDelay = 100 * time.Millisecond
	output := &cappedOutput{remaining: 16 * 1024}
	cmd.Stdout = output
	err := cmd.Run()
	if err == nil {
		var languages []string
		languages, err = parseAppleLanguages(output.String())
		if err == nil && len(languages) > 0 {
			return languages, nil
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// A missing/unreadable preference falls back to the message environment.
	if languages := localePreferences(os.Getenv); len(languages) > 0 {
		return languages, nil
	}
	return nil, err
}

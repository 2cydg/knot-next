package main

import (
	"context"
	"os"

	"knot-cli/internal/app"
	"knot-cli/internal/tui"
)

func main() {
	os.Exit(app.Run(context.Background(), os.Args[1:], app.Options{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Runner: func(ctx context.Context, launch app.Launch) int {
			return tui.Run(ctx, launch.Context, launch.Translator, os.Stdin, os.Stdout, os.Stderr)
		},
	}))
}

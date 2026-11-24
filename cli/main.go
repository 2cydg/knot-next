package main

import (
	"context"
	"os"

	"knot-cli/internal/app"
	"knot-cli/internal/corestop"
	"knot-cli/internal/entry"
	"knot-cli/internal/remoteexec"
	"knot-cli/internal/serverlist"
	"knot-cli/internal/tui"
)

func main() {
	os.Exit(app.Run(context.Background(), os.Args[1:], app.Options{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Runner: func(ctx context.Context, launch app.Launch) int {
			if launch.Context.Mode() == entry.StopCore {
				return corestop.Run(ctx, launch.Translator, os.Stdout, os.Stderr)
			}
			if launch.Context.Mode() == entry.Exec {
				return remoteexec.Run(ctx, launch.Context, launch.Translator, os.Stdout, os.Stderr)
			}
			if launch.Context.Mode() == entry.List {
				return serverlist.Run(ctx, launch.Context.ServerRef(), launch.Context.JSON(), launch.Translator, os.Stdout, os.Stderr)
			}
			return tui.Run(ctx, launch.Context, launch.Translator, os.Stdin, os.Stdout, os.Stderr)
		},
	}))
}

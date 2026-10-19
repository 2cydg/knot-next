package main

import (
	"context"
	"os"

	"knot-cli/internal/app"
)

func main() {
	os.Exit(app.Run(context.Background(), os.Args[1:], app.Options{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
	}))
}

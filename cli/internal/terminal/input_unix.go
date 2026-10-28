//go:build !windows

package terminal

import (
	"github.com/muesli/cancelreader"
	"os"
)

func newInput(input *os.File) (cancelreader.CancelReader, error) {
	return cancelreader.NewReader(input)
}
func prepareOutput(*os.File) (func(), error) { return func() {}, nil }

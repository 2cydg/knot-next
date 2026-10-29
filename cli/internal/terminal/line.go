package terminal

import (
	"context"
	"io"
	"os"
)

// ReadLine leaves native line editing and echo in charge. It reads one byte at
// a time so a confirmation cannot buffer input intended for the SSH bridge.
// File input is cancellable, and its cancellation worker joins before return.
func ReadLine(ctx context.Context, input io.Reader) (string, error) {
	if input == nil {
		return "", io.EOF
	}
	if file, ok := input.(*os.File); ok {
		reader, err := newInput(file)
		if err != nil {
			return "", err
		}
		defer reader.Close()
		stop, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			select {
			case <-ctx.Done():
				reader.Cancel()
			case <-stop:
			}
		}()
		defer func() { close(stop); <-done }()
		input = reader
	}
	var line []byte
	var b [1]byte
	for len(line) < 1024 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		_, err := io.ReadFull(input, b[:])
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err != nil {
			return "", err
		}
		if b[0] == '\n' {
			return string(line), nil
		}
		line = append(line, b[0])
	}
	return "", io.ErrShortBuffer
}

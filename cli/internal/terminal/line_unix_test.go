//go:build !windows

package terminal

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestReadLineCancellationJoinsReaderAndPreservesInput(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := ReadLine(ctx, input); done <- err }()
	// Let the file reader block with no complete input before interrupting it.
	timer := time.AfterFunc(20*time.Millisecond, cancel)
	defer timer.Stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("console reader survived cancellation")
	}
	if _, err := io.WriteString(writer, "next\n"); err != nil {
		t.Fatal(err)
	}
	line, err := ReadLine(context.Background(), input)
	if err != nil || line != "next" {
		t.Fatal("canceled confirmation retained stdin", line, err)
	}
}

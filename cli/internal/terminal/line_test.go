package terminal

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestReadLineDoesNotConsumeFollowingSSHInput(t *testing.T) {
	input := strings.NewReader("yes\nremote-command\n")
	line, err := ReadLine(context.Background(), input)
	remaining, _ := io.ReadAll(input)
	if err != nil || line != "yes" || string(remaining) != "remote-command\n" {
		t.Fatal("confirmation stole SSH input", line, string(remaining), err)
	}
}

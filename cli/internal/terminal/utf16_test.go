package terminal

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func TestConsoleUTF8Streaming(t *testing.T) {
	var c consoleUTF8
	units := utf16.Encode([]rune("中🙂文\x1b[A"))
	var got strings.Builder
	for _, u := range units {
		c.append([]uint16{u})
		var b [1]byte
		for n := c.read(b[:]); n > 0; n = c.read(b[:]) {
			got.Write(b[:n])
		}
	}
	if got.String() != "中🙂文\x1b[A" {
		t.Fatalf("got %q", got.String())
	}
}
func TestConsoleUTF8InvalidSurrogates(t *testing.T) {
	var c consoleUTF8
	c.append([]uint16{0xd800, 'x', 0xdc00})
	b := make([]byte, 32)
	n := c.read(b)
	if string(b[:n]) != "�x�" {
		t.Fatalf("got %q", b[:n])
	}
}

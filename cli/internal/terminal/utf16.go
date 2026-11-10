package terminal

import (
	"unicode/utf16"
	"unicode/utf8"
)

// consoleUTF8 converts a stream of UTF-16 code units, preserving a surrogate
// pair across reads and keeping excess UTF-8 bytes for small caller buffers.
type consoleUTF8 struct {
	high    uint16
	pending []byte
}

func (c *consoleUTF8) append(units []uint16) {
	for _, u := range units {
		if c.high != 0 {
			if u >= 0xdc00 && u <= 0xdfff {
				c.pending = utf8.AppendRune(c.pending, utf16.DecodeRune(rune(c.high), rune(u)))
				c.high = 0
				continue
			}
			c.pending = utf8.AppendRune(c.pending, utf8.RuneError)
			c.high = 0
		}
		switch {
		case u >= 0xd800 && u <= 0xdbff:
			c.high = u
		case u >= 0xdc00 && u <= 0xdfff:
			c.pending = utf8.AppendRune(c.pending, utf8.RuneError)
		default:
			c.pending = utf8.AppendRune(c.pending, rune(u))
		}
	}
}
func (c *consoleUTF8) read(dst []byte) int {
	n := copy(dst, c.pending)
	c.pending = c.pending[n:]
	return n
}

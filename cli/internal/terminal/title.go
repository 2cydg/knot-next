package terminal

import (
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// SetTitle changes the terminal tab/window title and icon name. Call it while
// the caller owns terminal output, outside Bubble Tea or bridge worker writes.
// Labels are treated as text so a server alias cannot inject terminal commands.
func SetTitle(output io.Writer, title string) error {
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, title)
	if file, ok := output.(*os.File); ok {
		// Windows consoles need VT processing enabled for OSC sequences.
		restore, err := prepareOutput(file)
		if err != nil {
			return err
		}
		defer restore()
	}
	sequence := ansi.SetIconNameWindowTitle(title)
	n, err := io.WriteString(output, sequence)
	if err == nil && n != len(sequence) {
		return io.ErrShortWrite
	}
	return err
}

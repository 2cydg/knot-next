package terminal

import (
	"bytes"
	"testing"
)

func TestSetTitleTreatsNamesAsPlainText(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"Knot", "Knot"},
		{"22", "22"},
		{"生产服务器 🔑", "生产服务器 🔑"},
		{"prod\x07\x1b\\\r\n\u009c\u202e", "prod\\"},
		{"", ""},
	} {
		var output bytes.Buffer
		if err := SetTitle(&output, tc.name); err != nil {
			t.Fatal(err)
		}
		if got, want := output.String(), "\x1b]0;"+tc.want+"\x07"; got != want {
			t.Fatalf("title sequence = %q, want %q", got, want)
		}
	}
}

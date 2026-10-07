package session

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestOSC7EverySplitPreservesBytes(t *testing.T) {
	for _, end := range []string{"\a", "\x1b\\"} {
		payload := []byte("\x00\xffprefix\x1b]7;file://remote/tmp/space%20%E4%B8%AD%E6%96%87" + end + "tail")
		for split := 0; split <= len(payload); split++ {
			var p osc7Parser
			var dirs []string
			for _, chunk := range [][]byte{payload[:split], payload[split:]} {
				out, got, _ := p.Observe(chunk)
				if !bytes.Equal(out, chunk) {
					t.Fatal("changed bytes")
				}
				dirs = append(dirs, got...)
			}
			if len(dirs) != 1 || dirs[0] != "/tmp/space 中文" {
				t.Fatalf("split %d dirs=%v", split, dirs)
			}
		}
		var got []string
		reader := newObservedReader(bytes.NewReader(payload), func(dir string) { got = append(got, dir) })
		out, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(out, payload) || len(got) != 1 {
			t.Fatal("reader altered binary output")
		}
	}
}
func TestOSC7RejectsInvalidAndBoundsUnterminated(t *testing.T) {
	for _, uri := range []string{"http://host/tmp", "file://host", "file://user@host/tmp", "file://host/tmp?x=y", "file://host/%00", "file://host/%1b", "file://host/%zz", "relative"} {
		var p osc7Parser
		_, dirs, _ := p.Observe([]byte("\x1b]7;" + uri + "\a"))
		if len(dirs) != 0 {
			t.Fatalf("accepted %q", uri)
		}
	}
	var p osc7Parser
	p.Observe([]byte("\x1b]7;file://host/"))
	for i := 0; i < 100; i++ {
		p.Observe(bytes.Repeat([]byte{'a'}, 4096))
		if len(p.buf) > osc7MaxBuffer {
			t.Fatal("unbounded buffer")
		}
	}
	_, dirs, _ := p.Observe([]byte("\a\x1b]7;file://host/ok\a"))
	if len(dirs) != 1 || dirs[0] != "/ok" {
		t.Fatal("failed to recover")
	}
	p = osc7Parser{}
	_, dirs, _ = p.Observe([]byte(strings.Repeat("noise", 2000) + "\x1b]7;file://host/large-read\a"))
	if len(dirs) != 1 {
		t.Fatal("large read lost valid observation")
	}
}
func FuzzOSC7(f *testing.F) {
	f.Add([]byte("\x00\x1b]7;file://host/a%20b\x1b\\"))
	f.Add([]byte("\x1b]7;broken"))
	f.Fuzz(func(t *testing.T, data []byte) {
		var p osc7Parser
		for start := 0; start < len(data); start += 17 {
			end := start + 17
			if end > len(data) {
				end = len(data)
			}
			out, _, _ := p.Observe(data[start:end])
			if !bytes.Equal(out, data[start:end]) || len(p.buf) > osc7MaxBuffer {
				t.Fatal("observer invariants")
			}
		}
	})
}

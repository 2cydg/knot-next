package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/coder/websocket"
)

type testFrame struct {
	kind websocket.MessageType
	data []byte
}
type recordedAttachment struct{ frames []testFrame }

func (s *recordedAttachment) Read(context.Context) (websocket.MessageType, []byte, error) {
	if len(s.frames) == 0 {
		return 0, nil, io.EOF
	}
	f := s.frames[0]
	s.frames = s.frames[1:]
	return f.kind, f.data, nil
}
func textFrame(value any) testFrame {
	b, _ := json.Marshal(value)
	return testFrame{websocket.MessageText, b}
}

func TestReceivePreservesBinaryAndDrainsAfterErrorEvent(t *testing.T) {
	data := bytes.Repeat([]byte{'\x1b', ']', '7', ';', 0, 0xff, '\r', '\n', 0xc0, 0x80}, 10000)
	s := &recordedAttachment{frames: []testFrame{
		textFrame(map[string]any{"type": "session.snapshot", "session": map[string]any{"id": "opaque"}}),
		textFrame(map[string]any{"type": "session.error", "session_id": "opaque"}),
		{websocket.MessageBinary, data[:111]},
		textFrame(map[string]any{"type": "session.attach.truncated", "session_id": "opaque"}),
		{websocket.MessageBinary, data[111:]},
		textFrame(map[string]any{"type": "session.exit", "session_id": "opaque", "exit_code": 7}),
	}}
	var out bytes.Buffer
	r := receive(context.Background(), s, "opaque", &out)
	if r.Err != nil || r.Code() != 7 || !r.Truncated || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("invalid raw result: %+v, bytes=%d", r, out.Len())
	}
}

func TestReceiveRequiresActualExitAndSessionIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		frames []testFrame
	}{
		{"missing-status", []testFrame{textFrame(map[string]any{"type": "session.exit", "session_id": "opaque"})}},
		{"missing-snapshot", []testFrame{textFrame(map[string]any{"type": "session.exit", "session_id": "opaque", "exit_code": 0})}},
		{"wrong-session", []testFrame{textFrame(map[string]any{"type": "session.snapshot", "session": map[string]any{"id": "other"}})}},
		{"broken-stream", nil},
		{"bad-json", []testFrame{{websocket.MessageText, []byte(`{`)}}},
		{"attach-error", []testFrame{textFrame(map[string]any{"type": "error", "code": "ATTACH_FAILED"})}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := receive(context.Background(), &recordedAttachment{frames: test.frames}, "opaque", io.Discard)
			if r.Err == nil || r.Code() != 1 {
				t.Fatalf("fabricated success: %+v", r)
			}
		})
	}
}

package coreclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Socket preserves text/binary opcodes and serializes all application writes.
// Future terminal/control owners share this writer instead of writing directly.
type Socket struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (s *Socket) Write(ctx context.Context, kind websocket.MessageType, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.conn.Write(ctx, kind, payload)
}
func (s *Socket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	return s.conn.Read(ctx)
}
func (s *Socket) Close() { _ = s.conn.CloseNow() }
func (c *Client) dial(ctx context.Context, path string) (*Socket, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.token)
	conn, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(c.base, "http")+path, &websocket.DialOptions{HTTPClient: c.http, HTTPHeader: header})
	if err != nil {
		if res != nil {
			if res.Body != nil {
				defer res.Body.Close()
			}
			if res.StatusCode == 401 {
				return nil, &Error{Code: "UNAUTHORIZED", Status: 401}
			}
			if res.Body != nil {
				var envelope struct {
					Error *Error `json:"error"`
				}
				if json.NewDecoder(io.LimitReader(res.Body, maxResponse)).Decode(&envelope) == nil && envelope.Error != nil && envelope.Error.Code != "" {
					envelope.Error.Status = res.StatusCode
					return nil, envelope.Error
				}
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, Failure("EVENTS_UNAVAILABLE")
	}
	conn.SetReadLimit(maxResponse)
	return &Socket{conn: conn}, nil
}

type StreamItem struct {
	Event Event
	Err   error
}
type Stream struct {
	Items  <-chan StreamItem
	cancel context.CancelFunc
	done   <-chan struct{}
}

func (s *Stream) Close() {
	if s != nil {
		s.cancel()
		<-s.done
	}
}

// Events uses a bounded, cancelable queue. A slow UI applies backpressure; if
// core closes a slow subscriber, the UI refreshes with GET rather than guessing.
func (c *Client) Events(ctx context.Context) (*Stream, error) {
	dialCtx, cancelDial := context.WithTimeout(ctx, 8*time.Second)
	defer cancelDial()
	socket, err := c.dial(dialCtx, "/v1/events")
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	items := make(chan StreamItem, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(items)
		defer socket.Close()
		first := true
		for {
			kind, data, err := socket.Read(streamCtx)
			item := StreamItem{}
			if err != nil {
				if streamCtx.Err() != nil {
					return
				}
				item.Err = Failure("EVENTS_UNAVAILABLE")
			} else if kind != websocket.MessageText || json.Unmarshal(data, &item.Event) != nil || item.Event.Type == "" {
				item.Err = Failure("PROTOCOL_ERROR")
			} else if first && (item.Event.Type != "core.snapshot" || item.Event.APIVersion != "v1") {
				item.Err = Failure("PROTOCOL_ERROR")
			}
			first = false
			select {
			case items <- item:
			case <-streamCtx.Done():
				return
			}
			if item.Err != nil {
				return
			}
		}
	}()
	return &Stream{Items: items, cancel: cancel, done: done}, nil
}

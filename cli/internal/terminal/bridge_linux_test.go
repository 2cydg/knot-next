package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"knot-cli/internal/coreclient"
)

func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		t.Fatal(err)
	}
	return master, slave
}

func TestNativeBridgeRepeatedRawBytesResizeTailAndRestore(t *testing.T) {
	master, slave := openPTY(t)
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	for round := range 3 {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			payload := bytes.Repeat([]byte{'X', 0, 0xff, '\r', '\n', '\x1b', '[', 'm'}, 20000)
			input := []byte{3, 4, 0, 0xff, '\r', '\n', '\x1b', '[', 'A'}
			serverDone := make(chan error, 1)
			ready := make(chan struct{})
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/runtime" {
					io.WriteString(w, `{"data":{"instance_id":"i"}}`)
					return
				}
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					serverDone <- err
					return
				}
				defer ws.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				write := func(v any) error { b, _ := json.Marshal(v); return ws.Write(ctx, websocket.MessageText, b) }
				if err := write(map[string]any{"type": "session.snapshot", "session": map[string]any{"id": "opaque"}}); err != nil {
					serverDone <- err
					return
				}
				close(ready)
				gotInput, resized := false, false
				for !gotInput || !resized {
					kind, b, err := ws.Read(ctx)
					if err != nil {
						serverDone <- err
						return
					}
					if kind == websocket.MessageBinary {
						if !bytes.Equal(b, input) {
							serverDone <- fmt.Errorf("input changed: %x", b)
							return
						}
						gotInput = true
						continue
					}
					var control coreclient.SessionControl
					_ = json.Unmarshal(b, &control)
					if control.Type == "resize" && control.Rows == 35 && control.Cols == 120 {
						resized = true
					}
				}
				_ = write(map[string]any{"type": "session.error", "session_id": "opaque"})
				if err := ws.Write(ctx, websocket.MessageBinary, payload); err != nil {
					serverDone <- err
					return
				}
				serverDone <- write(map[string]any{"type": "session.exit", "session_id": "opaque", "exit_code": 7})
			}))
			defer s.Close()
			client, err := coreclient.New(s.URL, "token")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			var captured bytes.Buffer
			var workers sync.WaitGroup
			workers.Go(func() { _, _ = io.CopyN(&captured, master, int64(len(payload)+len("\x1b[2J\x1b[H"))) })
			workers.Go(func() {
				<-ready
				// Wait for raw mode rather than racing the initial snapshot.
				for {
					state, _ := term.GetState(int(slave.Fd()))
					if !reflect.DeepEqual(state, before) {
						break
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Millisecond):
					}
				}
				_ = unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 35, Col: 120})
				_, _ = master.Write(input)
			})
			result := Run(ctx, client, "i", "opaque", slave, slave)
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
			workers.Wait()
			after, _ := term.GetState(int(slave.Fd()))
			if result.Err != nil || result.Code() != 7 || !reflect.DeepEqual(before, after) || !bytes.Equal(captured.Bytes(), append([]byte("\x1b[2J\x1b[H"), payload...)) {
				t.Fatalf("bridge result=%+v, terminal restored=%v, bytes=%d", result, reflect.DeepEqual(before, after), captured.Len())
			}
		})
	}
}

func TestNativeBridgeAttachConflictDoesNotChangeTerminal(t *testing.T) {
	_, slave := openPTY(t)
	before, _ := term.GetState(int(slave.Fd()))
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/runtime" {
			io.WriteString(w, `{"data":{"instance_id":"i"}}`)
			return
		}
		w.WriteHeader(409)
		io.WriteString(w, `{"error":{"code":"CONFLICT"}}`)
	}))
	defer s.Close()
	c, _ := coreclient.New(s.URL, "token")
	defer c.Close()
	r := Run(context.Background(), c, "i", "opaque", slave, slave)
	after, _ := term.GetState(int(slave.Fd()))
	if coreclient.Code(r.Err) != "CONFLICT" || !reflect.DeepEqual(before, after) {
		t.Fatal("attach conflict altered terminal", r)
	}
}

func TestNativeBridgeCancelJoinsBlockedInputAndRestores(t *testing.T) {
	master, slave := openPTY(t)
	before, _ := term.GetState(int(slave.Fd()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/runtime" {
			io.WriteString(w, `{"data":{"instance_id":"i"}}`)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_ = ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.snapshot","session":{"id":"opaque"}}`))
		_, _, _ = ws.Read(r.Context())
		cancel()
	}))
	defer s.Close()
	c, _ := coreclient.New(s.URL, "token")
	defer c.Close()
	done := make(chan Result, 1)
	go func() { done <- Run(ctx, c, "i", "opaque", slave, slave) }()
	select {
	case r := <-done:
		if r.Code() != 130 {
			t.Fatal(r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked stdin survived cancellation")
	}
	after, _ := term.GetState(int(slave.Fd()))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("raw mode leaked")
	}
	// A new reader must receive the next keystroke, rather than an old worker.
	_, _ = master.Write([]byte("next\n"))
	b := make([]byte, 32)
	n, err := slave.Read(b)
	if err != nil || string(b[:n]) != "next\n" {
		t.Fatal("old input reader stole input", err)
	}
}

func TestNativeBridgeDroppedWSReadsActualSessionResult(t *testing.T) {
	for _, state := range []string{"closed", "detached"} {
		t.Run(state, func(t *testing.T) {
			_, slave := openPTY(t)
			before, _ := term.GetState(int(slave.Fd()))
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/runtime":
					io.WriteString(w, `{"data":{"instance_id":"i"}}`)
				case "/v1/sessions/opaque":
					if state == "closed" {
						io.WriteString(w, `{"data":{"id":"opaque","state":"closed","exit_code":7}}`)
					} else {
						io.WriteString(w, `{"data":{"id":"opaque","state":"detached"}}`)
					}
				default:
					ws, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer ws.CloseNow()
					_ = ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.snapshot","session":{"id":"opaque"}}`))
					_, _, _ = ws.Read(r.Context())
				}
			}))
			defer s.Close()
			c, _ := coreclient.New(s.URL, "token")
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			r := Run(ctx, c, "i", "opaque", slave, slave)
			after, _ := term.GetState(int(slave.Fd()))
			if !reflect.DeepEqual(before, after) {
				t.Fatal("terminal not restored")
			}
			if state == "closed" {
				if r.Err != nil || r.Code() != 7 || !r.Truncated {
					t.Fatalf("GET exit result lost: %+v", r)
				}
			} else if !r.Detached || r.Code() != 1 {
				t.Fatalf("live session guessed finished: %+v", r)
			}
		})
	}
}

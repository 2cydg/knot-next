// Package terminal bridges native terminal bytes to the public attach protocol.
package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/term"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
)

type Result struct {
	ExitCode  *int
	Cause     string
	Truncated bool
	Detached  bool
	Err       error
}

func (r Result) Code() int {
	if errors.Is(r.Err, context.Canceled) {
		return entry.ExitCanceled
	}
	if r.Err != nil {
		return entry.ExitFailure
	}
	return entry.RemoteExitCode(r.ExitCode)
}

func Size(output *os.File) (rows, cols int) {
	cols, rows, err := term.GetSize(int(output.Fd()))
	if err != nil || rows < 1 || cols < 1 {
		return 24, 80
	}
	return min(rows, 1000), min(cols, 1000)
}

// Run returns only after the input and resize workers have stopped and native
// terminal state has been restored. Diagnostics are returned, never injected
// into remote output. A broken WS leaves a live session detached.
func Run(ctx context.Context, client *coreclient.Client, instance, id string, input, output *os.File) (result Result) {
	if input == nil || output == nil {
		return Result{Err: coreclient.Failure("TTY_REQUIRED")}
	}
	if err := client.CheckInstance(ctx, instance); err != nil {
		return Result{Err: err}
	}
	dialCtx, stopDial := context.WithTimeout(ctx, 8*time.Second)
	socket, err := client.Attach(dialCtx, id)
	stopDial()
	if err != nil {
		return Result{Err: err}
	}
	defer socket.Close()
	reader, err := newInput(input)
	if err != nil {
		return Result{Err: coreclient.Failure("TERMINAL_FAILED")}
	}
	defer reader.Close()
	restoreOutput, err := prepareOutput(output)
	if err != nil {
		return Result{Err: coreclient.Failure("TERMINAL_FAILED")}
	}
	defer restoreOutput()
	state, err := term.MakeRaw(int(input.Fd()))
	if err != nil {
		return Result{Err: coreclient.Failure("TERMINAL_FAILED")}
	}
	defer func() {
		if err := term.Restore(int(input.Fd()), state); err != nil && result.Err == nil {
			result.Err = coreclient.Failure("TERMINAL_FAILED")
		}
	}()
	// This release's fixed clear-on-connect default applies after the console
	// prompt or TUI has relinquished input, before remote bytes are written.
	if _, err := io.WriteString(output, "\x1b[2J\x1b[H"); err != nil {
		return Result{Err: err}
	}
	bridgeCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	workerError := make(chan error, 1)
	fail := func(err error) {
		select {
		case workerError <- err:
		default:
		}
		cancel()
	}
	write := func(kind websocket.MessageType, payload []byte) error {
		writeCtx, stop := context.WithTimeout(bridgeCtx, 8*time.Second)
		defer stop()
		return socket.Write(writeCtx, kind, payload)
	}
	control := func(c coreclient.SessionControl) error {
		payload, _ := json.Marshal(c)
		return write(websocket.MessageText, payload)
	}
	defer func() {
		cancel()
		reader.Cancel()
		socket.Close()
		workers.Wait()
	}()
	workers.Go(func() {
		buffer := make([]byte, 32<<10)
		for {
			n, err := reader.Read(buffer)
			if bridgeCtx.Err() != nil {
				return
			}
			if n > 0 {
				if err := write(websocket.MessageBinary, buffer[:n]); err != nil {
					fail(coreclient.Failure("ATTACH_LOST"))
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = control(coreclient.SessionControl{Type: "close_stdin"})
					if err != nil {
						err = coreclient.Failure("ATTACH_LOST")
					}
				} else {
					err = coreclient.Failure("TERMINAL_FAILED")
				}
				if err != nil {
					fail(err)
				}
				return
			}
		}
	})
	workers.Go(func() {
		timer := time.NewTicker(200 * time.Millisecond)
		defer timer.Stop()
		lastRows, lastCols := 0, 0
		for {
			rows, cols := Size(output)
			if rows != lastRows || cols != lastCols {
				if err := control(coreclient.SessionControl{Type: "resize", Rows: rows, Cols: cols}); err != nil {
					fail(coreclient.Failure("ATTACH_LOST"))
					return
				}
				lastRows, lastCols = rows, cols
			}
			select {
			case <-bridgeCtx.Done():
				return
			case <-timer.C:
			}
		}
	})
	result = receive(bridgeCtx, socket, id, output)
	if result.Err != nil {
		if ctx.Err() != nil {
			result.Err = ctx.Err()
			return result
		}
		select {
		case result.Err = <-workerError:
		default:
		}
		// Missing exit frame is never treated as success. Recover the actual
		// outcome via GET; a live session remains detached rather than deleted.
		getCtx, stop := context.WithTimeout(context.Background(), 8*time.Second)
		defer stop()
		if client.CheckInstance(getCtx, instance) == nil {
			if s, err := client.Session(getCtx, id); err == nil {
				if s.Done() {
					result.ExitCode, result.Cause = s.ExitCode, s.DisconnectCause
					// Output may be missing even with a known exit status.
					result.Truncated = true
					if coreclient.Code(result.Err) == "ATTACH_LOST" && s.ExitCode != nil && *s.ExitCode >= 0 && *s.ExitCode <= 255 && s.FrameworkCode == "" && (s.DisconnectCause == "" || s.DisconnectCause == "remote_signal") {
						result.Err = nil
					}
				} else {
					result.Detached = true
				}
			}
		}
	}
	return result
}

type attachment interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
}

func receive(ctx context.Context, socket attachment, id string, output io.Writer) Result {
	result := Result{}
	snapshot := false
	for {
		kind, bytes, err := socket.Read(ctx)
		if err != nil {
			result.Err = coreclient.Failure("ATTACH_LOST")
			return result
		}
		if kind == websocket.MessageBinary {
			if !snapshot {
				result.Err = coreclient.Failure("PROTOCOL_ERROR")
				return result
			}
			if n, err := output.Write(bytes); err != nil || n != len(bytes) {
				result.Err = coreclient.Failure("TERMINAL_FAILED")
				return result
			}
			continue
		}
		var frame struct {
			Type      string             `json:"type"`
			Session   coreclient.Session `json:"session"`
			SessionID string             `json:"session_id"`
			ExitCode  *int               `json:"exit_code"`
			Cause     string             `json:"disconnect_cause"`
			Code      string             `json:"code"`
		}
		if kind != websocket.MessageText || json.Unmarshal(bytes, &frame) != nil || frame.Type == "" {
			result.Err = coreclient.Failure("PROTOCOL_ERROR")
			return result
		}
		if frame.Type == "session.snapshot" {
			if frame.Session.ID != id {
				result.Err = coreclient.Failure("PROTOCOL_ERROR")
				return result
			}
			snapshot = true
			continue
		}
		if frame.SessionID != "" && frame.SessionID != id {
			result.Err = coreclient.Failure("PROTOCOL_ERROR")
			return result
		}
		switch frame.Type {
		case "session.exit":
			result.ExitCode, result.Cause = frame.ExitCode, frame.Cause
			if !snapshot || frame.ExitCode == nil || *frame.ExitCode < 0 || *frame.ExitCode > 255 || (frame.Cause != "" && frame.Cause != "remote_signal") {
				result.Err = coreclient.Failure("SSH_EXIT_UNKNOWN")
			}
			return result
		case "session.attach.truncated":
			result.Truncated = true
		case "error":
			if frame.Code == "" {
				frame.Code = "PROTOCOL_ERROR"
			}
			result.Err = coreclient.Failure(frame.Code)
			return result
			// State/error events can precede the final output. Only session.exit
			// ends the output stream; do not truncate its trailing bytes.
		}
	}
}

package terminal

import (
	"io"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// Own an overlapped console handle. Cancellation waits for the pending read
// before closing handles, so a subsequent TUI cannot race an old stdin reader.
type consoleInput struct {
	handle, stop windows.Handle
	once         sync.Once
}

func newInput(_ *os.File) (*consoleInput, error) {
	name, err := windows.UTF16PtrFromString("CONIN$")
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, err
	}
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	return &consoleInput{handle: handle, stop: stop}, nil
}
func (r *consoleInput) Read(buffer []byte) (int, error) {
	if state, _ := windows.WaitForSingleObject(r.stop, 0); state == windows.WAIT_OBJECT_0 {
		return 0, io.EOF
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(event)
	op := windows.Overlapped{HEvent: event}
	var n uint32
	err = windows.ReadFile(r.handle, buffer, &n, &op)
	if err != nil && err != windows.ERROR_IO_PENDING {
		return int(n), err
	}
	if err == nil {
		return int(n), nil
	}
	state, err := windows.WaitForMultipleObjects([]windows.Handle{event, r.stop}, false, windows.INFINITE)
	if err != nil || state == windows.WAIT_OBJECT_0+1 {
		_ = windows.CancelIoEx(r.handle, &op)
		_ = windows.GetOverlappedResult(r.handle, &op, &n, true)
		return 0, io.EOF
	}
	err = windows.GetOverlappedResult(r.handle, &op, &n, true)
	return int(n), err
}
func (r *consoleInput) Cancel() bool { r.once.Do(func() { _ = windows.SetEvent(r.stop) }); return true }
func (r *consoleInput) Close() error {
	r.Cancel()
	_ = windows.CloseHandle(r.stop)
	return windows.CloseHandle(r.handle)
}
func prepareOutput(output *os.File) (func(), error) {
	handle := windows.Handle(output.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return nil, err
	}
	if err := windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN); err != nil {
		return nil, err
	}
	return func() { _ = windows.SetConsoleMode(handle, mode) }, nil
}

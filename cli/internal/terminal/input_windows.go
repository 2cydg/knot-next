package terminal

import (
	"io"
	"os"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

var cancelSynchronousIO = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")

// ReadConsoleW preserves Unicode regardless of the console code page. The read
// runs on a locked OS thread so CancelSynchronousIo can interrupt it; callers
// join the input worker before handing the terminal to another reader.
type consoleInput struct {
	handle     windows.Handle
	mu         sync.Mutex
	canceled   bool
	thread     windows.Handle
	readDone   chan struct{}
	cancelDone chan struct{}
	decoder    consoleUTF8
}

func newInput(_ *os.File) (*consoleInput, error) {
	name, err := windows.UTF16PtrFromString("CONIN$")
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, err
	}
	return &consoleInput{handle: handle}, nil
}

func (r *consoleInput) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	if n := r.decoder.read(buffer); n > 0 {
		return n, nil
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	thread, err := windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(thread)
	r.mu.Lock()
	if r.canceled {
		r.mu.Unlock()
		return 0, io.EOF
	}
	done := make(chan struct{})
	r.thread, r.readDone = thread, done
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.thread = 0
		close(done)
		cancelDone := r.cancelDone
		r.mu.Unlock()
		if cancelDone != nil {
			<-cancelDone
		}
	}()
	for {
		// Limiting the read to the requested length keeps confirmation prompts from
		// consuming the following SSH input; surplus UTF-8 bytes remain buffered.
		units := make([]uint16, min(len(buffer), 4096))
		var n uint32
		err := windows.ReadConsole(r.handle, &units[0], uint32(len(units)), &n, nil)
		r.mu.Lock()
		canceled := r.canceled
		r.mu.Unlock()
		if canceled {
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		r.decoder.append(units[:n])
		if n := r.decoder.read(buffer); n > 0 {
			return n, nil
		}
	}
}

func (r *consoleInput) Cancel() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.canceled {
		return true
	}
	r.canceled = true
	if r.thread != 0 {
		thread, done := r.thread, r.readDone
		finished := make(chan struct{})
		r.cancelDone = finished
		go func() {
			defer close(finished)
			// Cancellation can race the start of ReadConsoleW. Retry until the read
			// has returned, without ever cancelling another thread's operation.
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				default:
				}
				cancelSynchronousIO.Call(uintptr(thread))
				select {
				case <-done:
					return
				case <-ticker.C:
				}
			}
		}()
	}
	return true
}
func (r *consoleInput) Close() error {
	r.Cancel()
	r.mu.Lock()
	done := r.cancelDone
	r.mu.Unlock()
	if done != nil {
		<-done
	}
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

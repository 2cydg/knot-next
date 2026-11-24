package upgrade

import (
	"bytes"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func watchProcess(pid int) (processWatch, string, error) {
	fd, err := unix.Kqueue()
	if err != nil {
		return processWatch{}, "", err
	}
	unix.CloseOnExec(fd)
	closeWatch := func() { _ = unix.Close(fd) }
	event := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(fd, []unix.Kevent_t{event}, nil, nil); err != nil {
		closeWatch()
		return processWatch{}, "", err
	}
	// KERN_PROCARGS2 returns an argc int followed by the NUL-terminated
	// executable path. The remaining bytes contain arguments and environment.
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(data) <= 4 {
		closeWatch()
		return processWatch{}, "", errors.Join(errors.New("could not read the executable path"), err)
	}
	image, _, _ := bytes.Cut(data[4:], []byte{0})
	watch := processWatch{
		exited: func() (bool, error) {
			var events [1]unix.Kevent_t
			n, err := unix.Kevent(fd, nil, events[:], &unix.Timespec{})
			if errors.Is(err, unix.EINTR) {
				return false, nil
			}
			return n > 0, err
		},
		close: closeWatch,
	}
	if exited, err := watch.exited(); exited || err != nil {
		closeWatch()
		return processWatch{}, "", errors.Join(os.ErrProcessDone, err)
	}
	return watch, string(image), nil
}

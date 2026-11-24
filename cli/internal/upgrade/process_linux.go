package upgrade

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func watchProcess(pid int) (processWatch, string, error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	// Read the start time before inspecting the image to catch PID reuse.
	state, started, err := processState(dir)
	if err != nil || state == "Z" || state == "X" {
		if err == nil {
			err = os.ErrProcessDone
		}
		return processWatch{}, "", err
	}
	image := filepath.Join(dir, "exe")
	if _, err := os.Stat(image); err != nil {
		return processWatch{}, "", err
	}
	watch := processWatch{
		exited: func() (bool, error) {
			state, current, err := processState(dir)
			if errors.Is(err, os.ErrNotExist) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			return current != started || state == "Z" || state == "X", nil
		},
		close: func() {},
	}
	if exited, err := watch.exited(); exited || err != nil {
		return processWatch{}, "", errors.Join(os.ErrProcessDone, err)
	}
	if fd, err := unix.PidfdOpen(pid, 0); err == nil {
		if exited, err := watch.exited(); exited || err != nil {
			_ = unix.Close(fd)
			return processWatch{}, "", errors.Join(os.ErrProcessDone, err)
		}
		watch = processWatch{
			exited: func() (bool, error) {
				n, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 0)
				if errors.Is(err, unix.EINTR) {
					return false, nil
				}
				return n > 0, err
			},
			close: func() { _ = unix.Close(fd) },
		}
	}
	return watch, image, nil
}

func processState(dir string) (string, string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return "", "", err
	}
	// The process name is enclosed in parentheses and can contain spaces or
	// parentheses itself. Start time is field 22, with state at field 3.
	fields := strings.Fields(string(data[strings.LastIndexByte(string(data), ')')+1:]))
	if len(fields) < 20 {
		return "", "", errors.New("invalid process status")
	}
	return fields[0], fields[19], nil
}

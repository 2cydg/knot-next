package upgrade

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func watchProcess(pid int) (processWatch, string, error) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return processWatch{}, "", err
	}
	image := make([]uint16, 32768)
	size := uint32(len(image))
	if err := windows.QueryFullProcessImageName(handle, 0, &image[0], &size); err != nil {
		_ = windows.CloseHandle(handle)
		return processWatch{}, "", err
	}
	return processWatch{
		exited: func() (bool, error) { return waitHandle(handle, 0) },
		close:  func() { _ = windows.CloseHandle(handle) },
	}, windows.UTF16ToString(image[:size]), nil
}

func waitHandle(handle windows.Handle, timeout uint32) (bool, error) {
	result, err := windows.WaitForSingleObject(handle, timeout)
	if err != nil {
		return false, err
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		return true, nil
	case uint32(windows.WAIT_TIMEOUT):
		return false, nil
	default:
		return false, fmt.Errorf("unexpected process wait result %d", result)
	}
}

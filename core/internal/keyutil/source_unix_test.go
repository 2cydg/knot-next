//go:build !windows

package keyutil

import (
	"errors"
	"os"
	"testing"
)

func TestReadSourceRejectsDeviceBeforeOpening(t *testing.T) {
	info, err := os.Stat("/dev/zero")
	if err != nil {
		t.Skip("no /dev/zero fixture")
	}
	if info.Mode().IsRegular() {
		t.Skip("/dev/zero is not a device")
	}
	if _, err := ReadSource("/dev/zero"); !errors.Is(err, ErrSourceNotRegular) {
		t.Fatal("device was not rejected", err)
	}
}

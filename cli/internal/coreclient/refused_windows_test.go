package coreclient

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsConnectionRefused(t *testing.T) {
	if !connectionRefused(fmt.Errorf("dial: %w", windows.WSAECONNREFUSED)) {
		t.Fatal("Winsock connection refusal was not recognized")
	}
	if connectionRefused(windows.WSAETIMEDOUT) {
		t.Fatal("timeout must not trigger auto-start")
	}
}

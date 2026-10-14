package resourceid

import (
	"errors"
	"testing"
)

func TestAllocateCollisionsAndEntropyFailure(t *testing.T) {
	n := 0
	id, err := Allocate("ssh", func() (string, error) {
		n++
		if n < 3 {
			return "same", nil
		}
		return "new", nil
	}, func(id string) bool { return id == "ssh_same" })
	if err != nil || id != "ssh_new" || n != 3 {
		t.Fatalf("collision retry: %s %v %d", id, err, n)
	}
	failure := errors.New("entropy unavailable")
	if _, err := Allocate("ssh", func() (string, error) { return "", failure }, func(string) bool { return false }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := Allocate("ssh", func() (string, error) { return "same", nil }, func(string) bool { return true }); err == nil {
		t.Fatal("unbounded collision retries")
	}
}

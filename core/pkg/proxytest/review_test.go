package proxytest

import (
	"context"
	"errors"
	"testing"
)

func TestCapacityAndStoppedAreDistinct(t *testing.T) {
	s := NewService(nil)
	for i := 0; i < cap(s.slots); i++ {
		s.slots <- struct{}{}
	}
	if _, err := s.Test(context.Background(), Request{}); !errors.Is(err, ErrLimit) {
		t.Fatal("capacity not rejected", err)
	}
	for len(s.slots) > 0 {
		<-s.slots
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Test(context.Background(), Request{}); !errors.Is(err, ErrStopped) {
		t.Fatal("shutdown misreported as capacity", err)
	}
}

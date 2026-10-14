package proxytest

import (
	"context"
	"knot-core/internal/testutil/proxyserver"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestCallerCancelAndShutdownCloseProbeSocket(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller", true: "shutdown"}[shutdown], func(t *testing.T) {
			fixture := proxyserver.New(t, proxyserver.Config{Type: "socks5", Stall: true})
			host, p, _ := net.SplitHostPort(fixture.Address())
			port, _ := strconv.Atoi(p)
			s := NewService(nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan Result, 1)
			go func() {
				r, err := s.Test(ctx, Request{Draft: &Draft{Type: "socks5", Host: host, Port: port}, TimeoutMS: 60000})
				if err != nil {
					t.Error(err)
				}
				done <- r
			}()
			deadline := time.Now().Add(time.Second)
			for fixture.ConnectionCount() == 0 {
				if time.Now().After(deadline) {
					t.Fatal("probe never connected")
				}
				time.Sleep(time.Millisecond)
			}
			if shutdown {
				budget, cancelBudget := context.WithTimeout(context.Background(), time.Second)
				defer cancelBudget()
				if err := s.Shutdown(budget); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case result := <-done:
				if result.FailureCode != "PROXY_TEST_CANCELED" {
					t.Fatal(result)
				}
			case <-time.After(time.Second):
				t.Fatal("probe ignored cancellation")
			}
			if err := s.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			deadline = time.Now().Add(time.Second)
			for fixture.ConnectionCount() != 0 {
				if time.Now().After(deadline) {
					t.Fatal("probe socket remained open")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

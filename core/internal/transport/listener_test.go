package transport

import (
	"net"
	"testing"
)

func TestValidateLoopbackHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1"} {
		if err := ValidateLoopbackHost(host); err != nil {
			t.Fatalf("ValidateLoopbackHost(%q) returned error: %v", host, err)
		}
	}
	for _, host := range []string{"0.0.0.0", "192.168.1.10", "example.com"} {
		if err := ValidateLoopbackHost(host); err == nil {
			t.Fatalf("ValidateLoopbackHost(%q) succeeded, want error", host)
		}
	}
}

func TestListenLoopbackUsesOnlyLoopbackAddresses(t *testing.T) {
	listeners, addresses, err := ListenLoopback(0)
	if err != nil {
		t.Fatalf("ListenLoopback returned error: %v", err)
	}
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()
	if len(listeners) == 0 {
		t.Fatal("no listeners created")
	}
	for i, ln := range listeners {
		addr := ln.Addr().(*net.TCPAddr)
		if !addr.IP.IsLoopback() {
			t.Fatalf("listener %d uses non-loopback address %s", i, addr)
		}
	}
	for _, addr := range addresses {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("invalid listen address %q: %v", addr, err)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			t.Fatalf("reported non-loopback address %q", addr)
		}
	}
}

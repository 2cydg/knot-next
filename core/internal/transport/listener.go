package transport

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

var loopbackHosts = []string{"127.0.0.1", "::1"}

func LoopbackHosts() []string {
	return append([]string(nil), loopbackHosts...)
}

func ValidateLoopbackHost(host string) error {
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("non-loopback listen address %q is not allowed", host)
	}
	return nil
}

func ListenLoopback(port int) ([]net.Listener, []string, error) {
	if port < 0 || port > 65535 {
		return nil, nil, fmt.Errorf("invalid port %d", port)
	}
	listeners := make([]net.Listener, 0, len(loopbackHosts))
	addresses := make([]string, 0, len(loopbackHosts))
	actualPort := port
	var errs []error
	for _, host := range loopbackHosts {
		// Keep this guard even though the current hosts are constants. It prevents
		// future edits to the listener set from weakening the loopback-only boundary.
		if err := ValidateLoopbackHost(host); err != nil {
			return nil, nil, err
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(actualPort)))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if actualPort == 0 {
			actualPort = ln.Addr().(*net.TCPAddr).Port
		}
		listeners = append(listeners, ln)
		addresses = append(addresses, ln.Addr().String())
	}
	if len(listeners) == 0 {
		return nil, nil, errors.Join(errs...)
	}
	return listeners, addresses, nil
}

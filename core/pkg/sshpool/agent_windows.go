//go:build windows

package sshpool

import (
	"errors"
	"net"
)

func defaultAgentSocket() string {
	return ""
}

func dialAgent(string) (net.Conn, error) {
	return nil, errors.New("ssh agent sockets are not supported on windows yet")
}

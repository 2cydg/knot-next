//go:build !windows

package sshpool

import (
	"net"
	"os"
)

func defaultAgentSocket() string {
	return os.Getenv("SSH_AUTH_SOCK")
}

func dialAgent(socket string) (net.Conn, error) {
	return net.Dial("unix", socket)
}

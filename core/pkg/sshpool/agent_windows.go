//go:build windows

package sshpool

import (
	"context"
	"net"
	"os"
	"strings"

	"github.com/Microsoft/go-winio"
)

// Adapted from knot/pkg/sshpool/agent_windows.go at e0b4d51eea6647e192371059381039b99fb301a2.
func defaultAgentSocket() string {
	if socket := os.Getenv("SSH_AUTH_SOCK"); socket != "" {
		return socket
	}
	return `\\.\pipe\openssh-ssh-agent`
}
func dialAgentContext(ctx context.Context, socket string) (net.Conn, error) {
	if strings.HasPrefix(strings.ToLower(socket), `\\.\pipe\`) {
		return winio.DialPipeContext(ctx, socket)
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", socket)
}

//go:build !windows

package sshpool

import (
	"context"
	"net"
	"os"
)

func defaultAgentSocket() string { return os.Getenv("SSH_AUTH_SOCK") }
func dialAgentContext(ctx context.Context, socket string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", socket)
}

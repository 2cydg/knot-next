package sshpool

import (
	"io"
	"net"
	"sync"

	"golang.org/x/crypto/ssh"
)

type reviewRouteConn struct {
	Release chan struct{}
	once    sync.Once
}

func (c *reviewRouteConn) User() string          { return "fake" }
func (c *reviewRouteConn) SessionID() []byte     { return nil }
func (c *reviewRouteConn) ClientVersion() []byte { return nil }
func (c *reviewRouteConn) ServerVersion() []byte { return nil }
func (c *reviewRouteConn) RemoteAddr() net.Addr  { return &net.TCPAddr{} }
func (c *reviewRouteConn) LocalAddr() net.Addr   { return &net.TCPAddr{} }
func (c *reviewRouteConn) SendRequest(string, bool, []byte) (bool, []byte, error) {
	return true, nil, nil
}
func (c *reviewRouteConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	return nil, nil, io.EOF
}
func (c *reviewRouteConn) Close() error { c.once.Do(func() { close(c.Release) }); return nil }
func (c *reviewRouteConn) Wait() error  { <-c.Release; return io.EOF }

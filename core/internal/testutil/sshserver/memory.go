package sshserver

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// NewMemory exercises the same SSH server over a buffered in-memory transport.
// It is complementary to New's TCP tests, never a fallback that skips them.
// Buffered writes allow both SSH peers to send their identification at once.
// SetDeadline/SetReadDeadline/SetWriteDeadline are no-ops on this transport;
// it verifies protocol and explicit control gates, not socket deadlines.
func NewMemory(t *testing.T, cfg Config) (*Server, *ssh.Client) {
	t.Helper()
	done := make(chan struct{})
	once := &sync.Once{}
	local := &memoryConn{input: make(chan []byte, 256), done: done, once: once}
	remote := &memoryConn{input: make(chan []byte, 256), done: done, once: once}
	local.peer, remote.peer = remote, local
	listener := &memoryListener{conn: remote, done: make(chan struct{})}
	srv := newServer(t, cfg, listener)
	conn, channels, requests, err := ssh.NewClientConn(local, "memory", &ssh.ClientConfig{
		User: cfg.User, Auth: []ssh.AuthMethod{ssh.Password(cfg.Password)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		_ = local.Close()
		t.Fatalf("memory SSH handshake: %v", err)
	}
	client := ssh.NewClient(conn, channels, requests)
	t.Cleanup(func() { _ = client.Close() })
	return srv, client
}

type memoryListener struct {
	conn     net.Conn
	accepted bool
	done     chan struct{}
	once     sync.Once
}

func (l *memoryListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *memoryListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *memoryListener) Addr() net.Addr { return memoryAddr("127.0.0.1:0") }

type memoryConn struct {
	input chan []byte
	peer  *memoryConn
	buf   []byte
	done  chan struct{}
	once  *sync.Once
}

func (c *memoryConn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		select {
		case c.buf = <-c.input:
		case <-c.done:
			return 0, io.EOF
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}
func (c *memoryConn) Write(p []byte) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	data := append([]byte(nil), p...)
	select {
	case c.peer.input <- data:
		return len(p), nil
	case <-c.done:
		return 0, net.ErrClosed
	}
}
func (c *memoryConn) Close() error                     { c.once.Do(func() { close(c.done) }); return nil }
func (c *memoryConn) LocalAddr() net.Addr              { return memoryAddr("local") }
func (c *memoryConn) RemoteAddr() net.Addr             { return memoryAddr("remote") }
func (c *memoryConn) SetDeadline(time.Time) error      { return nil }
func (c *memoryConn) SetReadDeadline(time.Time) error  { return nil }
func (c *memoryConn) SetWriteDeadline(time.Time) error { return nil }

type memoryAddr string

func (a memoryAddr) Network() string { return "memory" }
func (a memoryAddr) String() string  { return string(a) }

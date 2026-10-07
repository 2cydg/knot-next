package sftp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"knot-core/pkg/session"

	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type reviewSSHConn struct {
	ch   *reviewSSHChannel
	done chan struct{}
	once sync.Once
}

func (c *reviewSSHConn) User() string          { return "fake" }
func (c *reviewSSHConn) SessionID() []byte     { return nil }
func (c *reviewSSHConn) ClientVersion() []byte { return nil }
func (c *reviewSSHConn) ServerVersion() []byte { return nil }
func (c *reviewSSHConn) RemoteAddr() net.Addr  { return &net.TCPAddr{} }
func (c *reviewSSHConn) LocalAddr() net.Addr   { return &net.TCPAddr{} }
func (c *reviewSSHConn) SendRequest(string, bool, []byte) (bool, []byte, error) {
	return true, nil, nil
}
func (c *reviewSSHConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	return c.ch, c.ch.requests, nil
}
func (c *reviewSSHConn) Close() error { c.once.Do(func() { c.ch.Close(); close(c.done) }); return nil }
func (c *reviewSSHConn) Wait() error  { <-c.done; return io.EOF }

type reviewSSHChannel struct {
	version        *bytes.Reader
	versionEntered chan struct{}
	readOnce       sync.Once
	closed         chan struct{}
	requests       chan *ssh.Request
	once           sync.Once
	entered        chan struct{}
	barrier        chan struct{}
	closeBarrier   chan struct{}
	closeWrites    atomic.Int32
	closes         atomic.Int32
}

func (c *reviewSSHChannel) Read(p []byte) (int, error) {
	if c.versionEntered != nil {
		c.readOnce.Do(func() { close(c.versionEntered) })
		<-c.closed
		return 0, io.EOF
	}
	if c.version.Len() > 0 {
		return c.version.Read(p)
	}
	<-c.closed
	return 0, io.EOF
}
func (c *reviewSSHChannel) Write(p []byte) (int, error) { return len(p), nil }
func (c *reviewSSHChannel) Close() error {
	if c.closeBarrier != nil {
		<-c.closeBarrier
	}
	c.once.Do(func() { c.closes.Add(1); close(c.closed); close(c.requests) })
	return nil
}
func (c *reviewSSHChannel) CloseWrite() error { c.closeWrites.Add(1); return nil }
func (c *reviewSSHChannel) SendRequest(string, bool, []byte) (bool, error) {
	if c.entered != nil {
		close(c.entered)
	}
	if c.barrier != nil {
		select {
		case <-c.barrier:
		case <-c.closed:
			return false, io.EOF
		}
	}
	return true, nil
}
func (c *reviewSSHChannel) Stderr() io.ReadWriter { return &reviewSSHStderr{c} }

type reviewSSHStderr struct{ c *reviewSSHChannel }

func (s *reviewSSHStderr) Read([]byte) (int, error)    { <-s.c.closed; return 0, io.EOF }
func (s *reviewSSHStderr) Write(p []byte) (int, error) { return len(p), nil }
func reviewSSHClient(t *testing.T, blocked bool) (*ssh.Client, *reviewSSHChannel) {
	t.Helper()
	ch := &reviewSSHChannel{version: bytes.NewReader([]byte{0, 0, 0, 5, 2, 0, 0, 0, 3}), closed: make(chan struct{}), requests: make(chan *ssh.Request)}
	if blocked {
		ch.entered = make(chan struct{})
		ch.barrier = make(chan struct{})
	}
	c := &reviewSSHConn{ch: ch, done: make(chan struct{})}
	channels := make(chan ssh.NewChannel)
	close(channels)
	reqs := make(chan *ssh.Request)
	close(reqs)
	client := ssh.NewClient(c, channels, reqs)
	t.Cleanup(func() { client.Close() })
	return client, ch
}
func TestReviewCancelClosesOpenedSubsystemChannel(t *testing.T) {
	client, ch := reviewSSHClient(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := openSFTPSubsystem(ctx, client); done <- err }()
	<-ch.entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller did not exit")
	}
	if ch.closes.Load() == 0 {
		t.Fatal("caller returned on cancellation, but existing SSH channel remains open; constructor and cleanup goroutines still wait on subsystem response")
	}
}
func TestReviewCancelDuringVersionClosesChannel(t *testing.T) {
	client, ch := reviewSSHClient(t, false)
	ch.versionEntered = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := openSFTPSubsystem(ctx, client); done <- err }()
	select {
	case <-ch.versionEntered:
	case <-time.After(time.Second):
		t.Fatal("VERSION wait never entered")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("version cancellation blocked")
	}
	if ch.closes.Load() != 1 {
		t.Fatal("VERSION cancellation did not close channel")
	}
}

func TestReviewSFTPCloseClosesSessionChannel(t *testing.T) {
	client, ch := reviewSSHClient(t, false)
	sftpClient, err := openSFTPSubsystem(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(t.TempDir())
	svc.sessions["fake"] = &resource{Session: Session{ID: "fake", State: "open"}, client: sftpClient}
	done := make(chan error, 1)
	go func() { _, err := svc.Close("fake"); done <- err }()
	blocked := false
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		blocked = true
	}
	writes := ch.closeWrites.Load()
	closes := ch.closes.Load()
	if blocked {
		ch.Close()
		<-done
	}
	if blocked {
		t.Fatalf("SFTP Close blocked while shared SSH transport remained live: CloseWrite=%d, channel.Close=%d; only EOF was sent", writes, closes)
	}
}

type memoryConn struct {
	in     chan []byte
	peer   *memoryConn
	buf    []byte
	done   chan struct{}
	once   *sync.Once
	closed atomic.Bool
}

func memoryPipe() (net.Conn, net.Conn) {
	once := &sync.Once{}
	done := make(chan struct{})
	a := &memoryConn{in: make(chan []byte, 1024), done: done, once: once}
	b := &memoryConn{in: make(chan []byte, 1024), done: done, once: once}
	a.peer = b
	b.peer = a
	return a, b
}

func (c *memoryConn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		select {
		case data := <-c.in:
			c.buf = data
		case <-c.done:
			return 0, io.EOF
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *memoryConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	data := append([]byte(nil), p...)
	select {
	case c.peer.in <- data:
		return len(p), nil
	case <-c.done:
		return 0, io.ErrClosedPipe
	}
}

func (c *memoryConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.once.Do(func() {
		close(c.done)
	})
	return nil
}

func (c *memoryConn) LocalAddr() net.Addr {
	return memoryAddr("local")
}

func (c *memoryConn) RemoteAddr() net.Addr {
	return memoryAddr("remote")
}

func (c *memoryConn) SetDeadline(time.Time) error {
	return nil
}

func (c *memoryConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *memoryConn) SetWriteDeadline(time.Time) error {
	return nil
}

type memoryAddr string

func (a memoryAddr) Network() string {
	return "memory"
}

func (a memoryAddr) String() string {
	return string(a)
}

func TestReviewSFTPCloseOnRealProtocolWithoutRemoteClose(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	local, remote := memoryPipe()
	defer local.Close()
	defer remote.Close()
	go func() {
		conn, channels, requests, err := ssh.NewServerConn(remote, cfg)
		if err != nil {
			return
		}
		defer conn.Close()
		go ssh.DiscardRequests(requests)
		for incoming := range channels {
			channel, requests, err := incoming.Accept()
			if err != nil {
				continue
			}
			go func() {
				defer channel.Close()
				for req := range requests {
					if req.Type == "subsystem" {
						req.Reply(true, nil)
						go func() {
							srv, err := pkgsftp.NewServer(channel)
							if err == nil {
								srv.Serve()
							}
						}()
					} else {
						req.Reply(false, nil)
					}
				}
			}()
		}
	}()
	conn, channels, requests, err := ssh.NewClientConn(local, "memory", &ssh.ClientConfig{User: "fake", HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(conn, channels, requests)
	defer client.Close()
	sub, err := openSFTPSubsystem(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(t.TempDir())
	svc.sessions["fake"] = &resource{Session: Session{ID: "fake", State: "open"}, client: sub}
	done := make(chan error, 1)
	go func() { _, err := svc.Close("fake"); done <- err }()
	blocked := false
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		blocked = true
	}
	if blocked {
		client.Close()
		<-done
	}
	if !blocked {
		second, err := openSFTPSubsystem(context.Background(), client)
		if err != nil {
			t.Fatalf("shared SSH connection no longer usable: %v", err)
		}
		if err := second.Close(); err != nil {
			t.Fatalf("second close: %v", err)
		}
	}
	if blocked {
		t.Fatal("real SSH/SFTP over memory transport: Close waits forever for remote EOF because local channel.Close was never invoked")
	}
}

func TestReviewSubsystemStageDeadlineClosesChannel(t *testing.T) {
	previous := subsystemTimeout
	subsystemTimeout = 20 * time.Millisecond
	defer func() { subsystemTimeout = previous }()
	client, ch := reviewSSHClient(t, true)
	_, err := openSFTPSubsystem(context.Background(), client)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stage error=%v", err)
	}
	if ch.closes.Load() != 1 {
		t.Fatal("stage timeout did not close opened channel")
	}
}

func TestReviewSFTPCloseReportsChannelTimeout(t *testing.T) {
	previous := subsystemGrace
	subsystemGrace = 20 * time.Millisecond
	defer func() { subsystemGrace = previous }()
	client, ch := reviewSSHClient(t, false)
	sub, err := openSFTPSubsystem(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	ch.closeBarrier = release
	defer func() {
		close(release)
		if err := sub.Close(); err != nil && !errors.Is(err, session.ErrTeardownTimeout) {
			t.Errorf("final close: %v", err)
		}
	}()
	svc := NewService(t.TempDir())
	svc.sessions["fake"] = &resource{Session: Session{ID: "fake", State: "open"}, client: sub}
	_, err = svc.Close("fake")
	if !errors.Is(err, session.ErrTeardownTimeout) {
		t.Fatalf("Close error=%v, want timeout", err)
	}
}

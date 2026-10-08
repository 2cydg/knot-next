package sshpool

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"knot-core/internal/resourcepolicy"
	"knot-core/pkg/config"
)

type auditJumpConn struct {
	entered chan struct{}
	gate    chan struct{}
	done    chan struct{}
}

func (c *auditJumpConn) User() string          { return "u" }
func (c *auditJumpConn) SessionID() []byte     { return nil }
func (c *auditJumpConn) ClientVersion() []byte { return nil }
func (c *auditJumpConn) ServerVersion() []byte { return nil }
func (c *auditJumpConn) RemoteAddr() net.Addr  { return &net.TCPAddr{} }
func (c *auditJumpConn) LocalAddr() net.Addr   { return &net.TCPAddr{} }
func (c *auditJumpConn) SendRequest(string, bool, []byte) (bool, []byte, error) {
	return true, nil, nil
}
func (c *auditJumpConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	close(c.entered)
	<-c.gate
	return nil, nil, errors.New("released")
}
func (c *auditJumpConn) Close() error { close(c.done); return nil }
func (c *auditJumpConn) Wait() error  { <-c.done; return nil }
func TestRegressionJumpStageTimeout(t *testing.T) {
	conn := &auditJumpConn{entered: make(chan struct{}), gate: make(chan struct{}), done: make(chan struct{})}
	defer close(conn.gate)
	channels := make(chan ssh.NewChannel)
	close(channels)
	requests := make(chan *ssh.Request)
	close(requests)
	jump := ssh.NewClient(conn, channels, requests)
	defer jump.Close()
	done := make(chan error, 1)
	go func() {
		_, err := dialTransport(context.Background(), "host:22", config.ServerProfile{}, config.RuntimeConfig{}, jump, 30*time.Millisecond)
		done <- err
	}()
	<-conn.entered
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("expected stage deadline: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("jump stage did not time out")
	}
}

func TestJumpTimeoutRetainsLateWork(t *testing.T) {
	conn := &auditJumpConn{entered: make(chan struct{}), gate: make(chan struct{}), done: make(chan struct{})}
	channels := make(chan ssh.NewChannel)
	close(channels)
	requests := make(chan *ssh.Request)
	close(requests)
	jump := ssh.NewClient(conn, channels, requests)
	defer jump.Close()
	ctx, work := resourcepolicy.Scope(context.Background())
	_, err := dialTransport(ctx, "host:22", config.ServerProfile{}, config.RuntimeConfig{}, jump, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || work.Count() == 0 {
		t.Fatalf("late worker not tracked: %v %d", err, work.Count())
	}
	select {
	case <-conn.done:
		t.Fatal("shared jump was closed")
	default:
	}
	close(conn.gate)
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := work.Wait(waitCtx); err != nil {
		t.Fatal("late worker did not settle", err)
	}
}

func TestDialPrefixRetainedUntilLateCleanup(t *testing.T) {
	pool := NewPool()
	defer pool.CloseAll()
	prefix := &entry{}
	pool.mu.Lock()
	release := pool.retainPrefixLocked([]*entry{prefix})
	pool.mu.Unlock()
	ctx := resourcepolicy.WithGroup(context.Background(), &pool.workers)
	work := &resourcepolicy.Group{}
	work.Add()
	pool.releaseDialPrefix(ctx, work, release)
	pool.mu.Lock()
	refs := prefix.refCount
	pool.mu.Unlock()
	if refs != 1 {
		t.Fatal("pending dial released shared prefix")
	}
	work.Done()
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// Pool also owns its housekeeping loop; inspect the prefix with a deadline.
	for {
		pool.mu.Lock()
		refs = prefix.refCount
		pool.mu.Unlock()
		if refs == 0 {
			break
		}
		select {
		case <-waitCtx.Done():
			t.Fatal("late cleanup did not release prefix")
		case <-time.After(time.Millisecond):
		}
	}
}

package session

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"knot-core/pkg/config"

	"golang.org/x/crypto/ssh"
)

// Tests that change execCleanupGrace/execChannelTimeout must remain serial;
// do not use t.Parallel while these package-level budgets are overridden.

// execTestConn isolates library waits that a real peer can leave unanswered.
// Channel close deliberately does not complete the protocol until release.
type execTestConn struct {
	ch          *execTestChannel
	openStarted chan struct{}
	openGate    chan struct{}
	done        chan struct{}
	once        sync.Once
}

func (c *execTestConn) User() string                                           { return "test" }
func (c *execTestConn) SessionID() []byte                                      { return nil }
func (c *execTestConn) ClientVersion() []byte                                  { return nil }
func (c *execTestConn) ServerVersion() []byte                                  { return nil }
func (c *execTestConn) RemoteAddr() net.Addr                                   { return &net.TCPAddr{} }
func (c *execTestConn) LocalAddr() net.Addr                                    { return &net.TCPAddr{} }
func (c *execTestConn) SendRequest(string, bool, []byte) (bool, []byte, error) { return true, nil, nil }
func (c *execTestConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	if c.openStarted != nil {
		close(c.openStarted)
		<-c.openGate
	}
	return c.ch, c.ch.requests, nil
}
func (c *execTestConn) Close() error { c.once.Do(func() { c.ch.release(); close(c.done) }); return nil }
func (c *execTestConn) Wait() error  { <-c.done; return io.EOF }

type execTestChannel struct {
	requests    chan *ssh.Request
	started     chan struct{}
	closeCalled chan struct{}
	finish      chan struct{}
	closeOnce   sync.Once
	finishOnce  sync.Once
	startGate   chan struct{}
}

func (c *execTestChannel) Read([]byte) (int, error)    { <-c.finish; return 0, io.EOF }
func (c *execTestChannel) Write(p []byte) (int, error) { return len(p), nil }
func (c *execTestChannel) Close() error                { c.closeOnce.Do(func() { close(c.closeCalled) }); return nil }
func (c *execTestChannel) CloseWrite() error           { return nil }
func (c *execTestChannel) Stderr() io.ReadWriter       { return c }
func (c *execTestChannel) SendRequest(kind string, _ bool, _ []byte) (bool, error) {
	if kind == "exec" {
		close(c.started)
		if c.startGate != nil {
			select {
			case <-c.startGate:
			case <-c.finish:
				return false, io.EOF
			}
		}
	}
	return true, nil
}
func (c *execTestChannel) release() { c.finishOnce.Do(func() { close(c.finish); close(c.requests) }) }
func stalledExecClient(t *testing.T) (*ssh.Client, *execTestConn) {
	t.Helper()
	ch := &execTestChannel{requests: make(chan *ssh.Request), started: make(chan struct{}), closeCalled: make(chan struct{}), finish: make(chan struct{})}
	c := &execTestConn{ch: ch, done: make(chan struct{})}
	channels := make(chan ssh.NewChannel)
	close(channels)
	requests := make(chan *ssh.Request)
	close(requests)
	client := ssh.NewClient(c, channels, requests)
	t.Cleanup(func() { client.Close() })
	return client, c
}

func TestExecCleanupTimeoutTracksActualWorker(t *testing.T) {
	previous := execCleanupGrace
	execCleanupGrace = 20 * time.Millisecond
	defer func() { execCleanupGrace = previous }()
	client, conn := stalledExecClient(t)
	sess, err, opened := openExecSession(context.Background(), client)
	awaitExec(t, opened)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result  Exec
		settled <-chan struct{}
	}
	done := make(chan outcome, 1)
	go func() {
		result, settled := executeSSH(ctx, sess, Exec{Command: "wait"}, config.RuntimeConfig{})
		done <- outcome{result, settled}
	}()
	awaitExec(t, conn.ch.started)
	cancel()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup budget did not bound caller")
	}
	if got.result.FrameworkCode != "canceled" || got.result.CleanupError != "cleanup_timeout" {
		t.Fatalf("result=%+v", got.result)
	}
	awaitExec(t, conn.ch.closeCalled)
	select {
	case <-got.settled:
		t.Fatal("pending Wait reported as finished")
	default:
	}
	svc := NewService()
	operation := &execOperation{done: make(chan struct{})}
	svc.execActive["pending"] = operation
	go func() {
		<-got.settled
		svc.mu.Lock()
		close(operation.done)
		delete(svc.execActive, "pending")
		svc.mu.Unlock()
	}()
	budget, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := svc.ShutdownExec(budget); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown hid pending worker: %v", err)
	}
	stop()
	conn.ch.release()
	awaitExec(t, got.settled)
	if err := svc.ShutdownExec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestExecCanceledChannelOpenClosesLateResult(t *testing.T) {
	client, conn := stalledExecClient(t)
	conn.openStarted, conn.openGate = make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		err     error
		settled <-chan struct{}
	}
	done := make(chan outcome, 1)
	go func() { _, err, settled := openExecSession(ctx, client); done <- outcome{err, settled} }()
	awaitExec(t, conn.openStarted)
	cancel()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(time.Second):
		t.Fatal("channel open cancel blocked")
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatal(got.err)
	}
	select {
	case <-got.settled:
		t.Fatal("unconfirmed open claimed completion")
	default:
	}
	close(conn.openGate)
	awaitExec(t, got.settled)
	awaitExec(t, conn.ch.closeCalled)
	select {
	case <-conn.ch.started:
		t.Fatal("late channel started command")
	default:
	}
}

func TestExecChannelOpenHasStageDeadline(t *testing.T) {
	previous := execChannelTimeout
	execChannelTimeout = 20 * time.Millisecond
	defer func() { execChannelTimeout = previous }()
	client, conn := stalledExecClient(t)
	conn.openStarted, conn.openGate = make(chan struct{}), make(chan struct{})
	_, err, settled := openExecSession(context.Background(), client)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("channel deadline: %v", err)
	}
	select {
	case <-settled:
		t.Fatal("pending channel open marked settled")
	default:
	}
	close(conn.openGate)
	awaitExec(t, settled)
	awaitExec(t, conn.ch.closeCalled)
}

func TestExecCancelDuringStartWaitsForWorker(t *testing.T) {
	previous := execCleanupGrace
	execCleanupGrace = 20 * time.Millisecond
	defer func() { execCleanupGrace = previous }()
	client, conn := stalledExecClient(t)
	conn.ch.startGate = make(chan struct{})
	sess, err, opened := openExecSession(context.Background(), client)
	awaitExec(t, opened)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	returned := make(chan Exec, 1)
	go func() {
		result, settled := executeSSH(ctx, sess, Exec{Command: "wait"}, config.RuntimeConfig{})
		returned <- result
		<-settled
		close(done)
	}()
	awaitExec(t, conn.ch.started)
	cancel()
	awaitExec(t, conn.ch.closeCalled)
	// Closing the transport releases an unanswered exec request; only then can
	// the tracked execution goroutine actually finish.
	select {
	case result := <-returned:
		if result.CleanupError != "cleanup_timeout" {
			t.Fatalf("result=%+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup did not return within its budget")
	}
	conn.ch.release()
	awaitExec(t, done)
}

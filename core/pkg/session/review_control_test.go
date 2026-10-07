package session

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"knot-core/internal/testutil/sshserver"
)

// gatedStdio stands in for a session's stdin. Writes go nowhere, and Close parks
// until the test releases the gate: that is how a stdin close stuck in a network
// write is reproduced without a real stalled socket. It may be given an error to
// return, which is how a backend-side failure is injected.
type gatedStdio struct {
	mu       sync.Mutex
	closes   int
	closeErr error
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func newGatedStdio(closeErr error) *gatedStdio {
	return &gatedStdio{
		closeErr: closeErr,
		started:  make(chan struct{}, 8),
		release:  make(chan struct{}),
	}
}

func (g *gatedStdio) Write(p []byte) (int, error) { return len(p), nil }

func (g *gatedStdio) Close() error {
	g.mu.Lock()
	g.closes++
	g.mu.Unlock()
	g.started <- struct{}{}
	<-g.release
	return g.closeErr
}

func (g *gatedStdio) closeCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closes
}

func (g *gatedStdio) unblock() {
	g.once.Do(func() { close(g.release) })
}

func (g *gatedStdio) waitStarted(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never reached the backend", what)
	}
}

// useTestBackend replaces the test-mode backend factory for one test.
func useTestBackend(t *testing.T, factory func() *interactiveBackend) {
	t.Helper()
	previous := newSessionBackend
	newSessionBackend = factory
	t.Cleanup(func() { newSessionBackend = previous })
}

// gatedService builds a test-mode service whose sessions each get their own
// gated stdin, so every session's control path can be parked by the test.
func gatedService(t *testing.T, closeErr error) *Service {
	t.Helper()
	useTestBackend(t, func() *interactiveBackend {
		return &interactiveBackend{
			stdin:      newGatedStdio(closeErr),
			pump:       newOutputPump(),
			exitResult: newExitResult(),
		}
	})
	service := NewService()
	service.UseLocalTestBackend()
	return service
}

// gatedStdin returns the gate behind a session's stdin.
func gatedStdin(t *testing.T, service *Service, id string) *gatedStdio {
	t.Helper()
	service.mu.Lock()
	defer service.mu.Unlock()
	session, ok := service.sessions[id]
	if !ok {
		t.Fatalf("session %s not found", id)
	}
	stdin, ok := session.backend.stdin.(*gatedStdio)
	if !ok {
		t.Fatalf("session %s has stdin %T, want *gatedStdio", id, session.backend.stdin)
	}
	return stdin
}

func createControlTestSession(t *testing.T, service *Service, alias string, rows, cols int) Resource {
	t.Helper()
	created, err := service.Create(CreateRequest{ServerRef: "srv-1", Alias: alias, Rows: rows, Cols: cols})
	if err != nil {
		t.Fatalf("create session %s: %v", alias, err)
	}
	return created
}

// waitForSessionEvent drains a session's event stream until one matches.
func waitForSessionEvent(t *testing.T, events <-chan Event, match func(Event) bool, what string) Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("event stream closed before %s", what)
			}
			if match(event) {
				return event
			}
		case <-deadline:
			t.Fatalf("no %s was published", what)
		}
	}
}

// TestReviewControlDoesNotHoldServiceLockOnIO is the reproduction from the
// review: with a stdin close parked in the backend, the rest of the service —
// including a GET and a control on another session — must stay fully responsive.
// Before the fix every control operation ran while holding the service lock, so
// the observation below could not return until the parked close was released.
func TestReviewControlDoesNotHoldServiceLockOnIO(t *testing.T) {
	service := gatedService(t, nil)
	blocked := createControlTestSession(t, service, "blocked", 24, 80)
	other := createControlTestSession(t, service, "other", 24, 80)
	stdin := gatedStdin(t, service, blocked.ID)

	type result struct {
		resource Resource
		err      error
	}
	controlDone := make(chan result, 1)
	go func() {
		resource, err := service.Control(blocked.ID, ControlRequest{Type: "close_stdin"})
		controlDone <- result{resource, err}
	}()
	stdin.waitStarted(t, "close_stdin")

	// Nothing releases the gate below, so the observation completing at all is
	// the proof that no service lock is held across the parked I/O.
	observed := make(chan error, 1)
	go func() {
		start := time.Now()
		if _, err := service.Get(other.ID); err != nil {
			observed <- fmt.Errorf("get: %w", err)
			return
		}
		resized, err := service.Control(other.ID, ControlRequest{Type: "resize", Rows: 40, Cols: 120})
		if err != nil {
			observed <- fmt.Errorf("resize: %w", err)
			return
		}
		if resized.Rows != 40 || resized.Cols != 120 {
			observed <- fmt.Errorf("resize = %dx%d, want 40x120", resized.Rows, resized.Cols)
			return
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			observed <- fmt.Errorf("an unrelated session took %s while a control operation was parked", elapsed)
			return
		}
		observed <- nil
	}()

	select {
	case err := <-observed:
		if err != nil {
			t.Fatalf("a parked control operation blocked another session: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("another session could not be served while a control operation was parked")
	}

	// The parked operation must still finish once the backend answers, exactly once.
	stdin.unblock()
	select {
	case got := <-controlDone:
		if got.err != nil {
			t.Fatalf("close_stdin: %v", got.err)
		}
		if got.resource.State != "connected" {
			t.Fatalf("state = %q, want connected", got.resource.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close_stdin did not finish after the backend I/O completed")
	}
	if calls := stdin.closeCalls(); calls != 1 {
		t.Fatalf("stdin closed %d times, want 1", calls)
	}
}

// TestReviewFailedControlIsNotReportedAsSuccess covers a backend-side failure:
// the caller gets a definite error, the failure is on the event stream, and the
// session is not reported as if the operation had succeeded.
func TestReviewFailedControlIsNotReportedAsSuccess(t *testing.T) {
	service := gatedService(t, errors.New("write |1: broken pipe"))
	created := createControlTestSession(t, service, "failing", 24, 80)
	stdin := gatedStdin(t, service, created.ID)
	stdin.unblock()

	events, cancel, _, err := service.Subscribe(created.ID)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	resource, err := service.Control(created.ID, ControlRequest{Type: "close_stdin"})
	if err == nil {
		t.Fatal("a failed close_stdin was reported as success")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if resource.ID != "" {
		t.Fatalf("failed control returned resource %q, want no resource", resource.ID)
	}

	event := waitForSessionEvent(t, events, func(event Event) bool {
		return event.Type == "session.error" && event.Error != ""
	}, "session.error for the failed control")
	if event.SessionID != created.ID {
		t.Fatalf("error event session = %q, want %q", event.SessionID, created.ID)
	}

	live, err := service.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if live.State != "connected" {
		t.Fatalf("state = %q, want connected", live.State)
	}
	if live.Rows != 24 || live.Cols != 80 {
		t.Fatalf("size = %dx%d, want the unchanged 24x80", live.Rows, live.Cols)
	}
}

// deadSSHSession returns a started session whose transport has already been
// closed, so any request on it fails the way a broken connection does.
func deadSSHSession(t *testing.T, srv *sshserver.Server) *ssh.Session {
	t.Helper()
	client, err := ssh.Dial("tcp", srv.Addr(), &ssh.ClientConfig{
		User:            testSSHUser,
		Auth:            []ssh.AuthMethod{ssh.Password(testSSHPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		t.Fatalf("new session: %v", err)
	}
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		_ = client.Close()
		t.Fatalf("request pty: %v", err)
	}
	if err := session.Shell(); err != nil {
		_ = client.Close()
		t.Fatalf("shell: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close transport: %v", err)
	}
	return session
}

// TestReviewFailedResizeIsNotReportedAsSuccess covers the same contract on the
// resize path: a window change the transport refuses must not update the
// resource or publish session.resized.
func TestReviewFailedResizeIsNotReportedAsSuccess(t *testing.T) {
	srv := startSSHServer(t, sshserver.ShellBehavior{Scripted: true, ReadStdin: true, SendExitStatus: true})
	dead := deadSSHSession(t, srv)
	useTestBackend(t, func() *interactiveBackend {
		return &interactiveBackend{
			session:    dead,
			stdin:      newGatedStdio(nil),
			pump:       newOutputPump(),
			exitResult: newExitResult(),
		}
	})
	service := NewService()
	service.UseLocalTestBackend()
	created := createControlTestSession(t, service, "stale", 24, 80)

	events, cancel, _, err := service.Subscribe(created.ID)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	resource, err := service.Control(created.ID, ControlRequest{Type: "resize", Rows: 50, Cols: 160})
	if err == nil {
		t.Fatal("a resize the transport refused was reported as success")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if resource.ID != "" {
		t.Fatalf("failed resize returned resource %q, want no resource", resource.ID)
	}

	live, err := service.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if live.Rows != 24 || live.Cols != 80 {
		t.Fatalf("size = %dx%d, want the unchanged 24x80", live.Rows, live.Cols)
	}

	// A failed resize publishes the failure, never a resize.
	deadline := time.After(time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatal("event stream closed before the resize outcome")
			}
			if event.Type == "session.resized" {
				t.Fatalf("a failed resize published %+v", event)
			}
			if event.Type == "session.error" && event.Error != "" {
				return
			}
		case <-deadline:
			t.Fatal("no session.error was published for the failed resize")
		}
	}
}

// TestReviewControlIsBoundedWhenTheBackendStalls covers the bounded half of the
// contract: a control operation whose I/O never completes still returns, with a
// definite error, and leaves the session usable.
func TestReviewControlIsBoundedWhenTheBackendStalls(t *testing.T) {
	previousTimeout := controlIOTimeout
	controlIOTimeout = 50 * time.Millisecond
	t.Cleanup(func() { controlIOTimeout = previousTimeout })

	service := gatedService(t, nil)
	created := createControlTestSession(t, service, "stalled", 24, 80)
	stdin := gatedStdin(t, service, created.ID)

	done := make(chan error, 1)
	go func() {
		_, err := service.Control(created.ID, ControlRequest{Type: "close_stdin"})
		done <- err
	}()
	stdin.waitStarted(t, "close_stdin")

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stalled control operation was reported as success")
		}
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("error = %v, want ErrConflict", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control did not return once its I/O bound expired")
	}

	live, err := service.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if live.State != "connected" {
		t.Fatalf("state = %q, want connected", live.State)
	}
	stdin.unblock()
}

// TestReviewDisconnectIsNotPinnedByStalledControlIO covers the other half: a
// disconnect on the same session must land while the control I/O is parked.
func TestReviewDisconnectIsNotPinnedByStalledControlIO(t *testing.T) {
	service := gatedService(t, nil)
	created := createControlTestSession(t, service, "stuck", 24, 80)
	stdin := gatedStdin(t, service, created.ID)

	controlDone := make(chan error, 1)
	go func() {
		_, err := service.Control(created.ID, ControlRequest{Type: "close_stdin"})
		controlDone <- err
	}()
	stdin.waitStarted(t, "close_stdin")

	disconnected := make(chan Resource, 1)
	go func() {
		resource, _ := service.Disconnect(created.ID)
		disconnected <- resource
	}()

	select {
	case resource := <-disconnected:
		if resource.State != "closed" {
			t.Fatalf("state after disconnect = %q, want closed", resource.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect was pinned behind a stalled control operation")
	}

	// The parked operation then reports the session it actually found, not a live
	// one: the resource that comes back is the closed session.
	stdin.unblock()
	select {
	case err := <-controlDone:
		if err != nil {
			t.Fatalf("close_stdin: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked control never finished")
	}
	live, err := service.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if live.State != "closed" {
		t.Fatalf("state = %q, want closed", live.State)
	}
	if live.Rows != 24 || live.Cols != 80 {
		t.Fatalf("size = %dx%d, want the unchanged 24x80", live.Rows, live.Cols)
	}
}

// TestReviewTeardownIsBoundedWhenStdinCloseStalls covers teardown itself: a
// stdin close stuck in a network write holds the SSH channel's write lock, so
// waiting for it in-line would pin a disconnect — and anything else that needs
// teardown — behind a remote that stopped reading.
//
// The bounded wait is reported, not swallowed: a close that has not finished is
// not a completed release, and a shutdown that returned nil for it would claim
// a resource had been freed while its close worker was still running.
func TestReviewTeardownIsBoundedWhenStdinCloseStalls(t *testing.T) {
	previousGrace := teardownGrace
	teardownGrace = 50 * time.Millisecond
	t.Cleanup(func() { teardownGrace = previousGrace })

	gate := newGatedStdio(nil)
	backend := &interactiveBackend{stdin: gate, pump: newOutputPump(), exitResult: newExitResult()}

	done := make(chan error, 1)
	go func() { done <- backend.Close() }()
	gate.waitStarted(t, "teardown stdin close")

	select {
	case err := <-done:
		if !errors.Is(err, ErrTeardownTimeout) {
			t.Fatalf("Close() = %v, want %v", err, ErrTeardownTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("teardown was pinned by a stalled stdin close")
	}

	// Teardown stays idempotent, and the stalled handle is not closed twice.
	if err := backend.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}
	if calls := gate.closeCalls(); calls != 1 {
		t.Fatalf("stdin closed %d times, want 1", calls)
	}
	gate.unblock()
}

// TestReviewCloseStepsRunTogether covers the other half of the same teardown:
// the SSH session close is what unblocks a stdin write stuck on it, so a stalled
// stdin close must not hold the session close back. Running the steps in
// sequence would keep both stuck instead of releasing either.
func TestReviewCloseStepsRunTogether(t *testing.T) {
	gate := newGatedStdio(nil)
	sessionClosed := make(chan struct{})
	session := func() error {
		close(sessionClosed)
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- closeConcurrently(gate.Close, session) }()
	gate.waitStarted(t, "stdin close")

	select {
	case <-sessionClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled stdin close held the session close back")
	}

	gate.unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("closeConcurrently() = %v, want nil once both steps finish", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closeConcurrently did not return after the stalled step was released")
	}
}

// TestReviewCloseConcurrentlySkipsNilSteps checks that a backend owning only
// some of the handles still closes what it has, and that a failing step does not
// hide behind a successful one.
func TestReviewCloseConcurrentlySkipsNilSteps(t *testing.T) {
	failure := errors.New("close failed")
	ran := false
	err := closeConcurrently(nil, func() error { ran = true; return failure }, nil)
	if !ran {
		t.Fatal("closeConcurrently skipped a real step")
	}
	if !errors.Is(err, failure) {
		t.Fatalf("closeConcurrently() = %v, want %v", err, failure)
	}
	if err := closeConcurrently(nil, nil); err != nil {
		t.Fatalf("closeConcurrently() with no steps = %v, want nil", err)
	}
}

func TestReviewDisconnectReportsBackendCloseTimeout(t *testing.T) {
	previous := teardownGrace
	teardownGrace = 20 * time.Millisecond
	defer func() { teardownGrace = previous }()
	service := gatedService(t, nil)
	created := createControlTestSession(t, service, "blocked-close", 24, 80)
	stdin := gatedStdin(t, service, created.ID)
	defer stdin.unblock()
	closed, err := service.Disconnect(created.ID)
	if !errors.Is(err, ErrTeardownTimeout) {
		t.Fatalf("Disconnect error=%v, want teardown timeout", err)
	}
	if closed.State != "closed" {
		t.Fatalf("state=%s", closed.State)
	}
}

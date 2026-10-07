package session

import (
	"bytes"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/sshpool"
)

const (
	testSSHUser     = "testuser"
	testSSHPassword = "testpass"
)

// newSSHService wires a session service to a real, controlled SSH server on
// loopback. Nothing here reaches the network beyond 127.0.0.1.
func newSSHService(t *testing.T, srv *sshserver.Server) (*Service, string, *testConfigService) {
	t.Helper()

	serverID := "loopback"
	cfgService := &testConfigService{runtime: config.RuntimeConfig{
		Settings: config.Settings{KeepaliveInterval: "-1s"},
		Servers: map[string]config.ServerProfile{
			serverID: {
				ID:         serverID,
				Alias:      "loopback",
				Host:       srv.Host(),
				Port:       srv.Port(),
				User:       testSSHUser,
				AuthMethod: config.AuthMethodPassword,
				Password:   testSSHPassword,
			},
		},
	}}

	pool := sshpool.NewPool()
	t.Cleanup(func() { pool.CloseAll() })

	service := NewService()
	service.UseConfig(cfgService)
	service.UsePool(pool)
	return service, serverID, cfgService
}

// startSSHServer starts a controlled SSH server and cleans it up with the test.
func startSSHServer(t *testing.T, behavior sshserver.ShellBehavior) *sshserver.Server {
	t.Helper()
	srv := sshserver.New(t, sshserver.Config{User: testSSHUser, Password: testSSHPassword})
	srv.SetShellBehavior(behavior)
	t.Cleanup(func() {
		srv.Close()
		srv.Wait()
	})
	return srv
}

// createSession creates a session with an explicit host key policy so the flow
// never blocks on an interactive challenge, then waits for a state.
func createSession(t *testing.T, service *Service, serverID string, req CreateRequest) Resource {
	t.Helper()
	if req.ServerRef == "" {
		req.ServerRef = serverID
	}
	if req.HostKeyPolicy == "" {
		req.HostKeyPolicy = sshpool.HostKeyPolicyInsecureSkip
	}
	created, err := service.Create(req)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return created
}

// waitForState polls until the session reaches one of the wanted states. The
// deadline is a failure bound, not a way to infer completion.
func waitForState(t *testing.T, service *Service, id string, timeout time.Duration, states ...string) Resource {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last Resource
	for time.Now().Before(deadline) {
		current, err := service.Get(id)
		if err != nil {
			t.Fatalf("get session %s: %v", id, err)
		}
		last = current
		for _, state := range states {
			if current.State == state {
				return current
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("session %s did not reach %v within %s (last state %q, error %q)",
		id, states, timeout, last.State, last.FrameworkError)
	return last
}

// attachCollector attaches and drains both streams until the session exits.
type attachCollector struct {
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	outcome  ExitOutcome
	overflow bool
	done     chan struct{}
	stream   AttachStream
}

// attachAndDrain attaches and collects output until the session reports its exit
// outcome. When release is non-nil it is closed right after the attachment is
// established, which lets a scripted server hold the session open until the
// client is actually watching.
func attachAndDrain(t *testing.T, service *Service, id string, release chan struct{}) *attachCollector {
	t.Helper()
	stream, _, err := service.AttachStream(id)
	if err != nil {
		t.Fatalf("attach stream: %v", err)
	}
	if release != nil {
		close(release)
	}
	collector := &attachCollector{done: make(chan struct{}), stream: stream}

	go func() {
		defer close(collector.done)
		stdoutOpen, stderrOpen := true, true
		for stdoutOpen || stderrOpen {
			select {
			case chunk, ok := <-stream.Stdout:
				if !ok {
					stdoutOpen = false
					continue
				}
				collector.stdout.Write(chunk)
			case chunk, ok := <-stream.Stderr:
				if !ok {
					stderrOpen = false
					continue
				}
				collector.stderr.Write(chunk)
			}
		}
	}()

	select {
	case outcome := <-stream.Exit:
		collector.outcome = outcome
	case <-time.After(10 * time.Second):
		t.Fatal("session did not report an exit outcome")
	}

	select {
	case <-collector.done:
	case <-time.After(5 * time.Second):
		t.Fatal("output streams did not close after exit")
	}
	collector.overflow = isChanClosed(stream.Overflow)
	return collector
}

// waitForWindowChanges polls until the server has recorded want window-change
// requests. window-change is a one-way request (wantReply=false), so there is no
// reply to synchronize on; the deadline is a failure bound.
func waitForWindowChanges(t *testing.T, srv *sshserver.Server, want int, timeout time.Duration) []sshserver.WindowSize {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if changes := srv.WindowChanges(); len(changes) >= want {
			return changes
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("server recorded %d window-change requests, want %d", len(srv.WindowChanges()), want)
	return nil
}

// waitForSignals polls until the server has recorded want signal requests.
func waitForSignals(t *testing.T, srv *sshserver.Server, want int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if signals := srv.Signals(); len(signals) >= want {
			return signals
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("server recorded %d signals, want %d", len(srv.Signals()), want)
	return nil
}

func isChanClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestSSHSessionRecordsPTYAndEnvironmentRequests(t *testing.T) {
	srv := startSSHServer(t, sshserver.ShellBehavior{Scripted: true, ReadStdin: true, SendExitStatus: true})
	service, serverID, _ := newSSHService(t, srv)

	created := createSession(t, service, serverID, CreateRequest{
		Term: "xterm-256color",
		Rows: 24,
		Cols: 80,
		Env:  map[string]string{"LANG": "en_US.UTF-8", "COLORTERM": "truecolor"},
	})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	requests := srv.PTYRequests()
	if len(requests) != 1 {
		t.Fatalf("pty requests = %d, want 1", len(requests))
	}
	got := requests[0]
	if got.Term != "xterm-256color" {
		t.Errorf("term = %q, want xterm-256color", got.Term)
	}
	if got.Width != 80 || got.Height != 24 {
		t.Errorf("pty size = %dx%d, want 80x24", got.Width, got.Height)
	}

	modes := parseTerminalModes(t, got.Modes)
	if modes[ssh.ECHO] != 1 {
		t.Errorf("ECHO mode = %d, want 1", modes[ssh.ECHO])
	}
	if modes[ssh.TTY_OP_ISPEED] != 38400 || modes[ssh.TTY_OP_OSPEED] != 38400 {
		t.Errorf("pty speeds = %d/%d, want 38400/38400", modes[ssh.TTY_OP_ISPEED], modes[ssh.TTY_OP_OSPEED])
	}

	env := map[string]string{}
	for _, request := range srv.EnvRequests() {
		env[request.Name] = request.Value
	}
	if env["LANG"] != "en_US.UTF-8" || env["COLORTERM"] != "truecolor" {
		t.Errorf("env requests = %v, want LANG and COLORTERM", env)
	}
}

// parseTerminalModes decodes the RFC 4254 terminal modes blob: pairs of an
// opcode byte and a uint32 value, terminated by TTY_OP_END (opcode 0).
func parseTerminalModes(t *testing.T, raw []byte) map[uint8]uint32 {
	t.Helper()
	const ttyOpEnd uint8 = 0
	modes := map[uint8]uint32{}
	for i := 0; i+5 <= len(raw); i += 5 {
		opcode := raw[i]
		if opcode == ttyOpEnd {
			break
		}
		modes[opcode] = uint32(raw[i+1])<<24 | uint32(raw[i+2])<<16 | uint32(raw[i+3])<<8 | uint32(raw[i+4])
	}
	return modes
}

func TestSSHSessionPreservesPTYBytesExactly(t *testing.T) {
	// ANSI, OSC7, CR/LF, NUL, CJK and invalid UTF-8, delivered in many small
	// writes so the client has to reassemble across frames.
	payload := []byte("\x1b[1;32mok\x1b[0m\r\n" +
		"\x1b]7;file://localhost/home/clax\x07" +
		"a\rb\nc\x00\x00" +
		"中文终端输出" +
		string([]byte{0xf0, 0x28, 0x8c, 0x28}))

	release := make(chan struct{})
	srv := startSSHServer(t, sshserver.ShellBehavior{
		Scripted:       true,
		SendExitStatus: true,
		Writes:         []sshserver.ScriptedWrite{{Data: payload, ChunkSize: 7, Gate: release}},
	})
	service, serverID, _ := newSSHService(t, srv)
	created := createSession(t, service, serverID, CreateRequest{})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	collector := attachAndDrain(t, service, created.ID, release)

	if !bytes.Equal(collector.stdout.Bytes(), payload) {
		t.Fatalf("output = %q\nwant      %q", collector.stdout.Bytes(), payload)
	}
	if collector.overflow {
		t.Fatal("output was reported as truncated")
	}
	if !collector.outcome.Succeeded() {
		t.Fatalf("outcome = %+v, want clean exit", collector.outcome)
	}
}

func TestSSHSessionExitCodeIsRecordedAndReportedConsistently(t *testing.T) {
	for _, exitCode := range []int{0, 7} {
		t.Run(string(rune('0'+exitCode)), func(t *testing.T) {
			release := make(chan struct{})
			srv := startSSHServer(t, sshserver.ShellBehavior{
				Scripted:       true,
				SendExitStatus: true,
				ExitCode:       exitCode,
				Writes:         []sshserver.ScriptedWrite{{Data: []byte("output before exit"), Gate: release}},
			})
			service, serverID, _ := newSSHService(t, srv)
			created := createSession(t, service, serverID, CreateRequest{})
			waitForState(t, service, created.ID, 5*time.Second, "connected")

			collector := attachAndDrain(t, service, created.ID, release)

			if string(collector.stdout.Bytes()) != "output before exit" {
				t.Fatalf("output = %q, want all bytes before exit", collector.stdout.Bytes())
			}
			if collector.outcome.Code == nil || *collector.outcome.Code != exitCode {
				t.Fatalf("attach outcome code = %v, want %d", collector.outcome.Code, exitCode)
			}

			// GET must agree with what the attach stream reported.
			final := waitForState(t, service, created.ID, 5*time.Second, "closed")
			if final.ExitCode == nil || *final.ExitCode != exitCode {
				t.Fatalf("resource exit code = %v, want %d", final.ExitCode, exitCode)
			}
			if final.FrameworkError != "" {
				t.Fatalf("framework error = %q, want none for a remote exit", final.FrameworkError)
			}
			if collector.outcome.Succeeded() != (exitCode == 0) {
				t.Fatalf("Succeeded() = %v for exit code %d", collector.outcome.Succeeded(), exitCode)
			}
		})
	}
}

func TestSSHSessionWithoutExitStatusIsNotSuccess(t *testing.T) {
	release := make(chan struct{})
	srv := startSSHServer(t, sshserver.ShellBehavior{
		Scripted: true,
		Writes:   []sshserver.ScriptedWrite{{Data: []byte("ended abruptly"), Gate: release}},
	})
	service, serverID, _ := newSSHService(t, srv)
	created := createSession(t, service, serverID, CreateRequest{})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	collector := attachAndDrain(t, service, created.ID, release)

	if collector.outcome.Succeeded() {
		t.Fatal("a session without an exit status must not be reported as success")
	}
	if collector.outcome.Code != nil {
		t.Fatalf("code = %d, want nil when no exit status was received", *collector.outcome.Code)
	}
	if collector.outcome.DisconnectCause != causeExitStatusMissing {
		t.Fatalf("cause = %q, want %q", collector.outcome.DisconnectCause, causeExitStatusMissing)
	}

	final := waitForState(t, service, created.ID, 5*time.Second, "failed")
	if final.FrameworkError == "" {
		t.Fatal("missing exit status should carry a framework error")
	}
	if final.ExitCode != nil {
		t.Fatalf("resource exit code = %d, want nil", *final.ExitCode)
	}
}

func TestSSHSessionSignalExitIsNotReportedAsClean(t *testing.T) {
	release := make(chan struct{})
	srv := startSSHServer(t, sshserver.ShellBehavior{
		Scripted:   true,
		ExitSignal: "KILL",
		Writes:     []sshserver.ScriptedWrite{{Data: []byte("killed"), Gate: release}},
	})
	service, serverID, _ := newSSHService(t, srv)
	created := createSession(t, service, serverID, CreateRequest{})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	collector := attachAndDrain(t, service, created.ID, release)

	if collector.outcome.Succeeded() {
		t.Fatal("a signal-terminated session must not be reported as success")
	}
	if collector.outcome.DisconnectCause != causeRemoteSignal {
		t.Fatalf("cause = %q, want %q", collector.outcome.DisconnectCause, causeRemoteSignal)
	}
	// RFC 4254 exit-signal is surfaced as 128+signum, and the value is preserved.
	if collector.outcome.Code == nil || *collector.outcome.Code != 137 {
		t.Fatalf("exit code = %v, want 137 (128+SIGKILL)", collector.outcome.Code)
	}
}

func TestSSHSessionNetworkDropIsRecordedAsNetworkError(t *testing.T) {
	release := make(chan struct{})
	srv := startSSHServer(t, sshserver.ShellBehavior{
		Scripted:       true,
		SendExitStatus: true,
		Writes:         []sshserver.ScriptedWrite{{Data: []byte("partial")}},
		BeforeExit:     release,
	})
	service, serverID, _ := newSSHService(t, srv)
	created := createSession(t, service, serverID, CreateRequest{})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	// Drop the transport out from under the session, then release the server
	// side; the client must report a connection failure, never a clean exit.
	srv.DropConnections()
	close(release)

	final := waitForState(t, service, created.ID, 5*time.Second, "failed", "closed")
	if final.FrameworkError == "" {
		t.Fatalf("state = %q with no framework error, want a recorded failure", final.State)
	}
	if final.ExitCode != nil && *final.ExitCode == 0 {
		t.Fatal("a dropped connection must not be recorded as exit 0")
	}
}

func TestSSHSessionResizeSendsRealWindowChange(t *testing.T) {
	srv := startSSHServer(t, sshserver.ShellBehavior{Scripted: true, ReadStdin: true, SendExitStatus: true})
	service, serverID, _ := newSSHService(t, srv)
	created := createSession(t, service, serverID, CreateRequest{Rows: 24, Cols: 80})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	resized, err := service.Control(created.ID, ControlRequest{Type: "resize", Rows: 50, Cols: 160})
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	if resized.Rows != 50 || resized.Cols != 160 {
		t.Fatalf("resource size = %dx%d, want 50x160", resized.Rows, resized.Cols)
	}

	changes := waitForWindowChanges(t, srv, 1, 5*time.Second)
	if changes[0].Width != 160 || changes[0].Height != 50 {
		t.Fatalf("window change = %+v, want 160x50", changes[0])
	}

	// Repeated identical resizes are real requests and are forwarded again.
	if _, err := service.Control(created.ID, ControlRequest{Type: "resize", Rows: 50, Cols: 160}); err != nil {
		t.Fatalf("second resize: %v", err)
	}
	if changes := waitForWindowChanges(t, srv, 2, 5*time.Second); len(changes) != 2 {
		t.Fatalf("window changes = %d, want 2", len(changes))
	}
}

func TestSSHSessionRejectsInvalidResizeWithoutTouchingRemote(t *testing.T) {
	srv := startSSHServer(t, sshserver.ShellBehavior{Scripted: true, ReadStdin: true, SendExitStatus: true})
	service, serverID, _ := newSSHService(t, srv)
	created := createSession(t, service, serverID, CreateRequest{Rows: 24, Cols: 80})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	for _, dims := range []struct{ rows, cols int }{
		{0, 80},
		{24, 0},
		{-1, 80},
		{24, -1},
		{1001, 80},
		{24, 1001},
	} {
		if _, err := service.Control(created.ID, ControlRequest{Type: "resize", Rows: dims.rows, Cols: dims.cols}); err == nil {
			t.Fatalf("resize %dx%d was accepted, want a validation error", dims.rows, dims.cols)
		}
	}

	if changes := srv.WindowChanges(); len(changes) != 0 {
		t.Fatalf("invalid sizes reached the remote: %+v", changes)
	}
}

func TestSSHSessionCloseStdinIsIdempotent(t *testing.T) {
	srv := startSSHServer(t, sshserver.ShellBehavior{
		Scripted:       true,
		ReadStdin:      true,
		SendExitStatus: true,
		Writes:         []sshserver.ScriptedWrite{{Data: []byte("done")}},
	})
	service, serverID, _ := newSSHService(t, srv)
	created := createSession(t, service, serverID, CreateRequest{})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	for i := 0; i < 3; i++ {
		if _, err := service.Control(created.ID, ControlRequest{Type: "close_stdin"}); err != nil {
			t.Fatalf("close_stdin #%d: %v", i+1, err)
		}
	}

	// Closing stdin makes the scripted shell finish and report its exit status.
	final := waitForState(t, service, created.ID, 5*time.Second, "closed")
	if final.ExitCode == nil || *final.ExitCode != 0 {
		t.Fatalf("exit code = %v, want 0", final.ExitCode)
	}
}

func TestSSHSessionSignalIsForwardedAndValidated(t *testing.T) {
	srv := startSSHServer(t, sshserver.ShellBehavior{Scripted: true, ReadStdin: true, SendExitStatus: true})
	service, serverID, _ := newSSHService(t, srv)
	created := createSession(t, service, serverID, CreateRequest{})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	if _, err := service.Control(created.ID, ControlRequest{Type: "signal", Signal: "USR1"}); err != nil {
		t.Fatalf("signal USR1: %v", err)
	}
	if _, err := service.Control(created.ID, ControlRequest{Type: "signal", Signal: "NOPE"}); err == nil {
		t.Fatal("unsupported signal was accepted")
	}

	signals := waitForSignals(t, srv, 1, 5*time.Second)
	if len(signals) != 1 || signals[0] != "USR1" {
		t.Fatalf("signals = %v, want exactly one USR1", signals)
	}
}

func TestSSHSessionReattachHasOneOutputOwner(t *testing.T) {
	phaseOne := bytes.Repeat([]byte("first-phase|"), 32)
	phaseTwo := bytes.Repeat([]byte("second-phase|"), 48)
	gateOne := make(chan struct{})
	gateTwo := make(chan struct{})

	srv := startSSHServer(t, sshserver.ShellBehavior{
		Scripted:       true,
		SendExitStatus: true,
		Writes: []sshserver.ScriptedWrite{
			{Data: phaseOne, ChunkSize: 16, Gate: gateOne},
			{Data: phaseTwo, ChunkSize: 16, Gate: gateTwo},
		},
	})
	service, serverID, _ := newSSHService(t, srv)
	created := createSession(t, service, serverID, CreateRequest{})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	// First attachment consumes the first output phase, then detaches. Output
	// that arrives while nothing is attached is discarded, not buffered.
	first, _, err := service.AttachStream(created.ID)
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	close(gateOne)

	firstRead := readBytes(t, first.Stdout, len(phaseOne), 5*time.Second)
	first.Cancel()
	first.Release()

	if !bytes.Equal(firstRead, phaseOne) {
		t.Fatalf("first attachment read %q, want %q", firstRead, phaseOne)
	}

	// The second attachment owns the only reader now: it must receive exactly the
	// second phase, with no replay of the first and no duplicate bytes.
	second, _, err := service.AttachStream(created.ID)
	if err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	close(gateTwo)

	secondRead := readBytes(t, second.Stdout, len(phaseTwo), 5*time.Second)
	if !bytes.Equal(secondRead, phaseTwo) {
		t.Fatalf("second attachment read %q\nwant                 %q", secondRead, phaseTwo)
	}
	second.Cancel()
	second.Release()
}

// readBytes reads until at least want bytes arrived, or fails at the deadline.
func readBytes(t *testing.T, ch <-chan []byte, want int, timeout time.Duration) []byte {
	t.Helper()
	var collected bytes.Buffer
	deadline := time.Now().Add(timeout)
	for collected.Len() < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out with %d/%d bytes", collected.Len(), want)
		}
		select {
		case chunk, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed with %d/%d bytes", collected.Len(), want)
			}
			collected.Write(chunk)
		case <-time.After(10 * time.Millisecond):
		}
	}
	return collected.Bytes()
}

func TestSSHSessionRejectsSecondAttachment(t *testing.T) {
	srv := startSSHServer(t, sshserver.ShellBehavior{Scripted: true, ReadStdin: true, SendExitStatus: true})
	service, serverID, _ := newSSHService(t, srv)
	created := createSession(t, service, serverID, CreateRequest{})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	first, _, err := service.AttachStream(created.ID)
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	defer func() {
		first.Cancel()
		first.Release()
	}()

	if _, _, err := service.AttachStream(created.ID); err == nil {
		t.Fatal("second attach was accepted, want a conflict")
	}

	// The rejected attempt must not have disturbed the first attachment.
	state := waitForState(t, service, created.ID, 2*time.Second, "attached")
	if !state.Attached {
		t.Fatalf("session = %+v, want the first attachment intact", state)
	}

	// Releasing the first attachment frees the session for the next client.
	first.Cancel()
	first.Release()
	if _, _, err := service.AttachStream(created.ID); err != nil {
		t.Fatalf("attach after release: %v", err)
	}
}

func TestSSHSessionClientDisconnectIsDistinctFromRemoteExit(t *testing.T) {
	srv := startSSHServer(t, sshserver.ShellBehavior{Scripted: true, ReadStdin: true, SendExitStatus: true})
	service, serverID, _ := newSSHService(t, srv)
	created := createSession(t, service, serverID, CreateRequest{})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	stream, _, err := service.AttachStream(created.ID)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() {
		stream.Cancel()
		stream.Release()
	}()

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		<-stream.Exit
	}()

	if _, err := service.Disconnect(created.ID); err != nil {
		t.Fatalf("disconnect: %v", err)
	}

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("attach observers were not notified of the local close")
	}

	final := waitForState(t, service, created.ID, 5*time.Second, "closed")
	if final.DisconnectCause != causeClientDisconnected {
		t.Fatalf("cause = %q, want %q", final.DisconnectCause, causeClientDisconnected)
	}
	if final.ExitCode != nil {
		t.Fatalf("exit code = %d, want nil for a client-initiated close", *final.ExitCode)
	}

	// Repeated disconnects are idempotent and must not re-close or panic.
	first := final.ExitedAt
	for i := 0; i < 2; i++ {
		again, err := service.Disconnect(created.ID)
		if err != nil {
			t.Fatalf("repeat disconnect #%d: %v", i+1, err)
		}
		if again.State != "closed" {
			t.Fatalf("state after repeat disconnect = %q, want closed", again.State)
		}
		if again.ExitedAt == nil || first == nil || !again.ExitedAt.Equal(*first) {
			t.Fatal("repeat disconnect changed the terminal timestamp")
		}
	}
}

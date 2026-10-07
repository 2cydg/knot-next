package http

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knot-core/internal/auth"
	"knot-core/internal/paths"
	coreruntime "knot-core/internal/runtime"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/core"
	"knot-core/pkg/crypto"
	"knot-core/pkg/secret"
	"knot-core/pkg/session"
	"knot-core/pkg/sftp"
	"knot-core/pkg/sshpool"
)

const (
	attachTestUser     = "testuser"
	attachTestPassword = "testpass"
)

// newSSHBackedTestServer wires the API server to a session service that talks to
// a real, controlled SSH server on loopback, so attach tests exercise the real
// protocol instead of a fake backend.
func newSSHBackedTestServer(t *testing.T, sshSrv *sshserver.Server) (*Server, string) {
	t.Helper()
	startedAt := time.Unix(1000, 0).UTC()
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "config"), filepath.Join(root, "state"))
	provider := crypto.NewStaticProvider([]byte("test-key"))
	configService := config.NewService(layout, provider)

	profile, err := configService.CreateServer(config.ServerProfile{
		Alias:      "loopback",
		Host:       sshSrv.Host(),
		Port:       sshSrv.Port(),
		User:       attachTestUser,
		AuthMethod: config.AuthMethodPassword,
	})
	if err != nil {
		t.Fatalf("create server profile: %v", err)
	}
	if _, err := configService.SetServerPassword(profile.ID, attachTestPassword); err != nil {
		t.Fatalf("set server password: %v", err)
	}

	sharedPool := sshpool.NewPool()
	t.Cleanup(func() { sharedPool.CloseAll() })

	coreService := core.New(core.DefaultVersion, startedAt)
	coreService.UseConfig(configService)
	coreService.UseSecret(secret.NewService(configService, provider))

	sessionService := session.NewService()
	sessionService.UseConfig(configService)
	sessionService.UsePool(sharedPool)
	coreService.UseSession(sessionService)

	sftpService := sftp.NewService(filepath.Join(root, "sftp"))
	sftpService.UseConfig(configService)
	sftpService.UseSession(sessionService)
	sftpService.UsePool(sharedPool)
	coreService.UseSFTP(sftpService)
	coreService.UseSSHPool(sharedPool)

	runtimeInfo := coreruntime.NewInfo(core.DefaultVersion, core.APIVersion, "attach-test-instance", 17898, []string{"127.0.0.1:17898"}, layout, startedAt, true)
	if err := coreruntime.WriteInfo(layout.RuntimePath, runtimeInfo); err != nil {
		t.Fatalf("write runtime info: %v", err)
	}
	return NewServer(coreService, testRuntimeHolder(runtimeInfo), auth.NewVerifier("test-token"), auth.NewOriginChecker(nil)), profile.ID
}

// startAttachSSHServer starts a controlled SSH server cleaned up with the test.
func startAttachSSHServer(t *testing.T, behavior sshserver.ShellBehavior) *sshserver.Server {
	t.Helper()
	srv := sshserver.New(t, sshserver.Config{User: attachTestUser, Password: attachTestPassword})
	srv.SetShellBehavior(behavior)
	t.Cleanup(func() {
		srv.Close()
		srv.Wait()
	})
	return srv
}

// createAttachSession creates a session through the public API and waits until
// the SSH connection is established.
func createAttachSession(t *testing.T, server *Server, serverID string, extra string) session.Resource {
	t.Helper()
	body := `{"server_ref":` + strconvQuote(serverID) + `,"host_key_policy":"insecure-skip","term":"xterm-256color","rows":24,"cols":80`
	if extra != "" {
		body += "," + extra
	}
	body += "}"

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, authenticatedJSONRequest(http.MethodPost, "/v1/sessions", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data session.Resource `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current := getAttachSession(t, server, created.Data.ID)
		if current.State == "connected" {
			return current
		}
		if current.State == "failed" {
			t.Fatalf("session failed to connect: %+v", current)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("session %s did not connect within 10s", created.Data.ID)
	return session.Resource{}
}

func getAttachSession(t *testing.T, server *Server, id string) session.Resource {
	t.Helper()
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, authenticatedRequest(http.MethodGet, "/v1/sessions/"+id))
	if rec.Code != http.StatusOK {
		t.Fatalf("get session status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Data session.Resource `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	return got.Data
}

// waitForAttachState polls the session until it reaches one of the states.
func waitForAttachState(t *testing.T, server *Server, id string, timeout time.Duration, states ...string) session.Resource {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last session.Resource
	for time.Now().Before(deadline) {
		last = getAttachSession(t, server, id)
		for _, state := range states {
			if last.State == state {
				return last
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("session %s did not reach %v within %s (last %q)", id, states, timeout, last.State)
	return last
}

// attachFrames is what one attach WebSocket delivered.
type attachFrames struct {
	binary     []byte
	text       []map[string]any
	exitAt     int // index into text of the session.exit frame, -1 when absent
	sawClose   bool
	closedConn bool

	// binaryAfterExit counts output frames that arrived after the session's
	// termination message. The frame order is the contract: every byte the remote
	// produced precedes the exit, so anything after it would be a byte delivered
	// to a client that has already been told the session ended.
	binaryAfterExit int
}

func (a attachFrames) exitFrame() map[string]any {
	if a.exitAt < 0 {
		return nil
	}
	return a.text[a.exitAt]
}

// readAttachFrames reads frames until the server ends the attachment.
func readAttachFrames(t *testing.T, conn net.Conn, reader *bufio.Reader) attachFrames {
	t.Helper()
	result := attachFrames{exitAt: -1}
	for {
		opcode, payload, err := readServerFrame(reader)
		if err != nil {
			result.closedConn = true
			return result
		}
		switch opcode {
		case wsOpcodeBinary:
			if result.exitAt >= 0 {
				result.binaryAfterExit++
			}
			result.binary = append(result.binary, payload...)
		case wsOpcodeText:
			var event map[string]any
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Fatalf("decode text frame %q: %v", payload, err)
			}
			result.text = append(result.text, event)
			if event["type"] == "session.exit" {
				result.exitAt = len(result.text) - 1
			}
		case wsOpcodeClose:
			result.sawClose = true
			return result
		case wsOpcodePing:
			// The server may ping; keep reading.
		case wsOpcodePong:
		default:
			t.Fatalf("unexpected opcode %d", opcode)
		}
	}
}

// assertAbnormalEnd requires that a session ended without being reported as a
// clean remote exit, and that any recorded cause is one of the plausible ones.
// A teardown and the transport error it causes race, so which of them is
// observed first is not fixed; both are accurate descriptions.
func assertAbnormalEnd(t *testing.T, final session.Resource, wantCauses ...string) {
	t.Helper()
	if final.State != "closed" && final.State != "failed" {
		t.Fatalf("state = %q, want a terminal state", final.State)
	}
	succeeded := final.FrameworkError == "" && final.ExitCode != nil && *final.ExitCode == 0
	if succeeded {
		t.Fatalf("session was reported as a clean exit: %+v", final)
	}
	if final.DisconnectCause == "" {
		return
	}
	for _, want := range wantCauses {
		if final.DisconnectCause == want {
			return
		}
	}
	t.Fatalf("disconnect cause = %q, want one of %v", final.DisconnectCause, wantCauses)
}

// readInitialAttachFrames consumes the two frames an attach always starts with:
// the session snapshot and the attach confirmation. They are queued first, so no
// output can precede them.
func readInitialAttachFrames(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	for _, want := range []string{"session.snapshot", "session.attached"} {
		opcode, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read %s frame: %v", want, err)
		}
		if opcode != wsOpcodeText {
			t.Fatalf("opcode = %d for %s, want text", opcode, want)
		}
		var frame map[string]any
		if err := json.Unmarshal(payload, &frame); err != nil {
			t.Fatalf("decode %s frame: %v", want, err)
		}
		if frame["type"] != want {
			t.Fatalf("frame type = %v, want %s", frame["type"], want)
		}
	}
}

// textFrame returns the first text frame of the given type.
func textFrame(frames attachFrames, frameType string) map[string]any {
	for _, frame := range frames.text {
		if frame["type"] == frameType {
			return frame
		}
	}
	return nil
}

func TestAttachForwardsPTYBytesExactly(t *testing.T) {
	payload := []byte("\x1b[1;31mwarning\x1b[0m\r\n" +
		"\x1b]7;file://localhost/var/log\x07" +
		"line1\r\nline2\rmid\n" +
		"\x00\x00" +
		"中文终端" +
		string([]byte{0xf0, 0x28, 0x8c, 0x28}))

	gate := make(chan struct{})
	sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{
		Scripted:       true,
		SendExitStatus: true,
		Writes:         []sshserver.ScriptedWrite{{Data: payload, ChunkSize: 7, Gate: gate}},
	})
	server, serverID := newSSHBackedTestServer(t, sshSrv)
	created := createAttachSession(t, server, serverID, "")

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	conn, reader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 20*time.Second)
	readInitialAttachFrames(t, reader)

	// Attach is established, so release the scripted output.
	close(gate)
	frames := readAttachFrames(t, conn, reader)

	if !bytes.Equal(frames.binary, payload) {
		t.Fatalf("PTY bytes = %q\nwant        %q", frames.binary, payload)
	}
	if textFrame(frames, "session.exit") == nil {
		t.Fatalf("no session.exit frame in %v", frames.text)
	}

	// OSC7 is observed for CWD tracking without consuming any bytes.
	final := waitForAttachState(t, server, created.ID, 5*time.Second, "closed")
	if final.CurrentDir != "/var/log" {
		t.Fatalf("current dir = %q, want /var/log", final.CurrentDir)
	}
}

func TestAttachDeliversTrailingOutputBeforeExit(t *testing.T) {
	// Both a clean exit and a failure code: the ordering contract is the same, and
	// exit 0 is the case a client is most likely to treat as final.
	for _, exitCode := range []int{0, 7} {
		t.Run(fmt.Sprintf("exit_%d", exitCode), func(t *testing.T) {
			// Larger than the relay and writer buffers, so the client has to keep
			// reading while the remote is still writing.
			payload := bytes.Repeat([]byte("0123456789abcdef"), 64*1024) // 1 MiB
			gate := make(chan struct{})
			sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{
				Scripted:       true,
				SendExitStatus: true,
				ExitCode:       exitCode,
				Writes:         []sshserver.ScriptedWrite{{Data: payload, ChunkSize: 4096, Gate: gate}},
			})
			server, serverID := newSSHBackedTestServer(t, sshSrv)
			created := createAttachSession(t, server, serverID, "")

			ts := httptest.NewServer(server.Handler())
			defer ts.Close()
			// The deadline is only a failure bound: a megabyte moves through the
			// client, the writer queue and the pump, and instrumentation or a loaded
			// machine must not turn that into a spurious timeout.
			conn, reader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 120*time.Second)
			readInitialAttachFrames(t, reader)
			close(gate)

			frames := readAttachFrames(t, conn, reader)

			if len(frames.binary) != len(payload) {
				t.Fatalf("received %d bytes, want %d", len(frames.binary), len(payload))
			}
			if !bytes.Equal(frames.binary, payload) {
				t.Fatal("trailing output did not match the bytes the remote sent")
			}
			if frames.exitAt < 0 {
				t.Fatalf("no session.exit frame in %v", frames.text)
			}
			if frames.binaryAfterExit != 0 {
				t.Fatalf("%d output frames arrived after session.exit", frames.binaryAfterExit)
			}
			// Exactly one exit message, and it is a termination message.
			exitCount := 0
			for _, frame := range frames.text {
				if frame["type"] == "session.exit" {
					exitCount++
				}
			}
			if exitCount != 1 {
				t.Fatalf("session.exit delivered %d times, want 1", exitCount)
			}
			gotCode, ok := frames.exitFrame()["exit_code"].(float64)
			if !ok || int(gotCode) != exitCode {
				t.Fatalf("exit frame = %v, want exit_code %d", frames.exitFrame(), exitCode)
			}
			if !frames.sawClose && !frames.closedConn {
				t.Fatal("attachment did not end after the exit message")
			}

			// GET must agree with what the attach stream reported.
			final := waitForAttachState(t, server, created.ID, 5*time.Second, "closed")
			if final.ExitCode == nil || *final.ExitCode != exitCode {
				t.Fatalf("resource exit code = %v, want %d", final.ExitCode, exitCode)
			}
		})
	}
}

func TestAttachRejectsSecondClientWhileFirstStaysUsable(t *testing.T) {
	// The default shell echoes input, which makes "the first client still works"
	// observable.
	sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{})
	server, serverID := newSSHBackedTestServer(t, sshSrv)
	created := createAttachSession(t, server, serverID, "")

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 10*time.Second)
	readInitialAttachFrames(t, reader)

	if _, err := conn.Write(maskedClientFrame(wsOpcodeBinary, []byte("hello"))); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	echo := readBinaryBytes(t, reader, len("hello"), 5*time.Second)
	if string(echo) != "hello" {
		t.Fatalf("echo = %q, want hello", echo)
	}

	// A second client is rejected before the upgrade, with a plain HTTP status.
	_, _, status := tryOpenTestWebSocket(t, ts, "/v1/sessions/"+created.ID+"/attach", 5*time.Second)
	if !strings.Contains(status, "409") {
		t.Fatalf("second attach status = %q, want 409 Conflict", status)
	}

	// The rejection must not disturb the first attachment.
	if _, err := conn.Write(maskedClientFrame(wsOpcodeBinary, []byte("again"))); err != nil {
		t.Fatalf("write stdin after rejection: %v", err)
	}
	echo = readBinaryBytes(t, reader, len("again"), 5*time.Second)
	if string(echo) != "again" {
		t.Fatalf("echo after rejection = %q, want again", echo)
	}

	attached := getAttachSession(t, server, created.ID)
	if !attached.Attached || attached.State != "attached" {
		t.Fatalf("session = %+v, want the first attachment intact", attached)
	}
}

// readBinaryBytes reads binary frames until want bytes arrived.
func readBinaryBytes(t *testing.T, reader *bufio.Reader, want int, timeout time.Duration) []byte {
	t.Helper()
	var collected bytes.Buffer
	deadline := time.Now().Add(timeout)
	for collected.Len() < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out with %d/%d bytes", collected.Len(), want)
		}
		opcode, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read frame: %v (have %d/%d bytes)", err, collected.Len(), want)
		}
		switch opcode {
		case wsOpcodeBinary:
			collected.Write(payload)
		case wsOpcodeClose:
			t.Fatalf("connection closed with %d/%d bytes", collected.Len(), want)
		default:
			// Events and pings are not output; ignore them here.
		}
	}
	return collected.Bytes()
}

func TestAttachDetachReattachKeepsSingleOwner(t *testing.T) {
	phaseOne := bytes.Repeat([]byte("first-phase|"), 16)
	phaseTwo := bytes.Repeat([]byte("second-phase|"), 24)
	gateOne := make(chan struct{})
	gateTwo := make(chan struct{})

	sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{
		Scripted:       true,
		SendExitStatus: true,
		Writes: []sshserver.ScriptedWrite{
			{Data: phaseOne, ChunkSize: 16, Gate: gateOne},
			{Data: phaseTwo, ChunkSize: 16, Gate: gateTwo},
		},
	})
	server, serverID := newSSHBackedTestServer(t, sshSrv)
	created := createAttachSession(t, server, serverID, "")

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	firstConn, firstReader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 20*time.Second)
	readInitialAttachFrames(t, firstReader)
	close(gateOne)

	firstBytes := readBinaryBytes(t, firstReader, len(phaseOne), 5*time.Second)
	if !bytes.Equal(firstBytes, phaseOne) {
		t.Fatalf("first attachment read %q, want %q", firstBytes, phaseOne)
	}

	// Detach: the client closes the WebSocket, and the session becomes free.
	if _, err := firstConn.Write(maskedClientFrame(wsOpcodeClose, nil)); err != nil {
		t.Fatalf("send close: %v", err)
	}
	_ = firstConn.Close()
	waitForAttachState(t, server, created.ID, 5*time.Second, "detached")

	// Re-attach: exactly one output owner, no replay of the first phase.
	secondConn, secondReader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 20*time.Second)
	defer secondConn.Close()
	readInitialAttachFrames(t, secondReader)
	close(gateTwo)

	secondBytes := readBinaryBytes(t, secondReader, len(phaseTwo), 5*time.Second)
	if !bytes.Equal(secondBytes, phaseTwo) {
		t.Fatalf("second attachment read %q\nwant                  %q", secondBytes, phaseTwo)
	}

	// The shell has finished, so the second attachment ends with the exit message.
	frames := readAttachFrames(t, secondConn, secondReader)
	if frames.exitAt < 0 {
		t.Fatalf("no session.exit after re-attach: %v", frames.text)
	}
	if len(frames.binary) != 0 {
		t.Fatalf("re-attach received %d duplicated bytes", len(frames.binary))
	}
}

func TestAttachControlMessages(t *testing.T) {
	sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{Scripted: true, ReadStdin: true, SendExitStatus: true})
	server, serverID := newSSHBackedTestServer(t, sshSrv)
	created := createAttachSession(t, server, serverID, "")

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 10*time.Second)
	readInitialAttachFrames(t, reader)

	sendControl := func(payload string) {
		if _, err := conn.Write(maskedClientFrame(wsOpcodeText, []byte(payload))); err != nil {
			t.Fatalf("write control: %v", err)
		}
	}
	// expectControlError sends a control message and requires a rejection frame.
	expectControlError := func(payload string) {
		sendControl(payload)
		if _, errFrame := readControlResult(t, reader); errFrame == nil {
			t.Fatalf("control %s was accepted, want an error frame", payload)
		}
	}

	// A valid resize reaches the remote and is reported on the session.
	sendControl(`{"type":"resize","rows":50,"cols":160}`)
	if event, errFrame := readControlResult(t, reader); errFrame != nil {
		t.Fatalf("resize rejected: %v", errFrame)
	} else if event == nil || event["type"] != "session.resized" {
		t.Fatalf("resize event = %v, want session.resized", event)
	}
	changes := waitForRecordedWindowChanges(t, sshSrv, 1, 5*time.Second)
	if changes[0].Width != 160 || changes[0].Height != 50 {
		t.Fatalf("remote window change = %+v, want 160x50", changes[0])
	}

	// Illegal sizes are rejected and never reach the remote.
	for _, payload := range []string{
		`{"type":"resize","rows":0,"cols":80}`,
		`{"type":"resize","rows":24,"cols":0}`,
		`{"type":"resize","rows":1001,"cols":80}`,
		`{"type":"resize","rows":24,"cols":-5}`,
	} {
		expectControlError(payload)
	}
	if changes := sshSrv.WindowChanges(); len(changes) != 1 {
		t.Fatalf("invalid sizes reached the remote: %+v", changes)
	}

	// A supported signal is forwarded: it changes no session state, so success is
	// verified against what the remote actually received.
	sendControl(`{"type":"signal","signal":"USR1"}`)
	signals := waitForRecordedSignals(t, sshSrv, 1, 5*time.Second)
	if len(signals) != 1 || signals[0] != "USR1" {
		t.Fatalf("remote signals = %v, want one USR1", signals)
	}
	expectControlError(`{"type":"signal","signal":"NOPE"}`)
	if signals := sshSrv.Signals(); len(signals) != 1 {
		t.Fatalf("unsupported signal reached the remote: %v", signals)
	}

	// close_stdin is idempotent: sending it twice must not close anything twice,
	// and it ends the scripted shell.
	sendControl(`{"type":"close_stdin"}`)
	sendControl(`{"type":"close_stdin"}`)
	frames := readAttachFrames(t, conn, reader)
	if frames.exitAt < 0 {
		t.Fatalf("no session.exit after close_stdin: %v", frames.text)
	}
}

// readControlResult reads until a control message produces either an event or an
// error frame, returning whichever it saw.
func readControlResult(t *testing.T, reader *bufio.Reader) (event map[string]any, errFrame map[string]any) {
	t.Helper()
	for {
		opcode, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read control result: %v", err)
		}
		if opcode != wsOpcodeText {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal(payload, &frame); err != nil {
			t.Fatalf("decode frame %q: %v", payload, err)
		}
		switch frame["type"] {
		case "error":
			return nil, frame
		case "session.resized", "session.snapshot", "session.attached", "session.cwd":
			return frame, nil
		}
	}
}

func waitForRecordedWindowChanges(t *testing.T, srv *sshserver.Server, want int, timeout time.Duration) []sshserver.WindowSize {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if changes := srv.WindowChanges(); len(changes) >= want {
			return changes
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("server recorded %d window changes, want %d", len(srv.WindowChanges()), want)
	return nil
}

func waitForRecordedSignals(t *testing.T, srv *sshserver.Server, want int, timeout time.Duration) []string {
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

func TestAttachInvalidResizeOverHTTPIsRejected(t *testing.T) {
	sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{Scripted: true, ReadStdin: true, SendExitStatus: true})
	server, serverID := newSSHBackedTestServer(t, sshSrv)
	created := createAttachSession(t, server, serverID, "")

	for _, body := range []string{
		`{"type":"resize","rows":0,"cols":80}`,
		`{"type":"resize","rows":1001,"cols":80}`,
		`{"type":"signal","signal":"NOPE"}`,
		`{"type":"bogus"}`,
	} {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, authenticatedJSONRequest(http.MethodPost, "/v1/sessions/"+created.ID+"/control", body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("control %s status = %d, want 400", body, rec.Code)
		}
	}
	if changes := sshSrv.WindowChanges(); len(changes) != 0 {
		t.Fatalf("invalid control reached the remote: %+v", changes)
	}

	// A valid resize is accepted and reflected on the resource.
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, authenticatedJSONRequest(http.MethodPost, "/v1/sessions/"+created.ID+"/control", `{"type":"resize","rows":50,"cols":160}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid resize status = %d, body = %s", rec.Code, rec.Body.String())
	}
	updated := getAttachSession(t, server, created.ID)
	if updated.Rows != 50 || updated.Cols != 160 {
		t.Fatalf("resource size = %dx%d, want 50x160", updated.Rows, updated.Cols)
	}
	if changes := waitForRecordedWindowChanges(t, sshSrv, 1, 5*time.Second); changes[0].Width != 160 {
		t.Fatalf("remote window change = %+v", changes[0])
	}
}

func TestAttachSlowClientEndsBoundedAndKeepsSessionAlive(t *testing.T) {
	// Deliberately far larger than every buffer between the remote and the client,
	// so the server cannot simply absorb it.
	payload := bytes.Repeat([]byte("slow-client-payload|"), 1024*1024)
	gate := make(chan struct{})
	hold := make(chan struct{})
	sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{
		Scripted:       true,
		SendExitStatus: true,
		Writes:         []sshserver.ScriptedWrite{{Data: payload, ChunkSize: 4096, Gate: gate}},
		BeforeExit:     hold,
	})
	server, serverID := newSSHBackedTestServer(t, sshSrv)
	created := createAttachSession(t, server, serverID, "")

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	_, reader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 60*time.Second)
	readInitialAttachFrames(t, reader)

	// The client stops reading entirely while the remote floods it.
	close(gate)

	// The server must end the attachment on its own: the client would otherwise
	// have to buffer without limit. Session state is the server-side proof.
	waitForAttachState(t, server, created.ID, 30*time.Second, "detached")

	// Whatever was buffered is bounded, and the stream ends rather than
	// delivering the whole flood.
	received := 0
	for {
		opcode, payload, err := readServerFrame(reader)
		if err != nil {
			break
		}
		if opcode == wsOpcodeClose {
			break
		}
		if opcode == wsOpcodeBinary {
			received += len(payload)
		}
	}
	if received >= len(payload) {
		t.Fatalf("received the entire %d byte flood, want a bounded amount", len(payload))
	}

	// The session itself survives its attachment being closed.
	final := getAttachSession(t, server, created.ID)
	if final.State == "failed" || final.State == "closed" {
		t.Fatalf("session state = %q, want the backend still alive", final.State)
	}
}

func TestAttachClientDisconnectKeepsSessionUsable(t *testing.T) {
	sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{})
	server, serverID := newSSHBackedTestServer(t, sshSrv)
	created := createAttachSession(t, server, serverID, "")

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	conn, reader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 10*time.Second)
	readInitialAttachFrames(t, reader)
	_ = conn.Close()

	waitForAttachState(t, server, created.ID, 5*time.Second, "detached")

	// The backend is still running, so the session can be attached again.
	second, secondReader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 10*time.Second)
	defer second.Close()
	readInitialAttachFrames(t, secondReader)
	if _, err := second.Write(maskedClientFrame(wsOpcodeBinary, []byte("still here"))); err != nil {
		t.Fatalf("write after re-attach: %v", err)
	}
	echo := readBinaryBytes(t, secondReader, len("still here"), 5*time.Second)
	if string(echo) != "still here" {
		t.Fatalf("echo after re-attach = %q", echo)
	}
}

func TestAttachRemoteNetworkDropReportsFailure(t *testing.T) {
	release := make(chan struct{})
	sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{
		Scripted:       true,
		ReadStdin:      true,
		SendExitStatus: true,
		Writes:         []sshserver.ScriptedWrite{{Data: []byte("partial")}},
		BeforeExit:     release,
	})
	server, serverID := newSSHBackedTestServer(t, sshSrv)
	created := createAttachSession(t, server, serverID, "")

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 15*time.Second)
	readInitialAttachFrames(t, reader)

	sshSrv.DropConnections()
	close(release)

	frames := readAttachFrames(t, conn, reader)
	exit := frames.exitFrame()
	if exit == nil {
		t.Fatalf("no session.exit after a dropped connection: %v", frames.text)
	}
	if code, ok := exit["exit_code"]; ok && code != nil {
		t.Fatalf("dropped connection reported exit_code %v, want none", code)
	}

	final := waitForAttachState(t, server, created.ID, 5*time.Second, "failed", "closed")
	assertAbnormalEnd(t, final, "network_error", "exit_status_missing", "pool_disconnected")
}

func TestAttachEndsWhenPoolDisconnects(t *testing.T) {
	sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{Scripted: true, ReadStdin: true, SendExitStatus: true})
	server, serverID := newSSHBackedTestServer(t, sshSrv)
	created := createAttachSession(t, server, serverID, "")

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 15*time.Second)
	readInitialAttachFrames(t, reader)

	// Clearing connections is the service-side teardown path: every pooled
	// connection is closed and dependent sessions must end, not hang.
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, authenticatedRequest(http.MethodPost, "/v1/connections/clear"))
	if rec.Code != http.StatusOK {
		t.Fatalf("clear connections status = %d, body = %s", rec.Code, rec.Body.String())
	}

	frames := readAttachFrames(t, conn, reader)
	if frames.exitFrame() == nil {
		t.Fatalf("no session.exit after the pool disconnected: %v", frames.text)
	}
	final := waitForAttachState(t, server, created.ID, 5*time.Second, "closed", "failed")
	assertAbnormalEnd(t, final, "pool_disconnected", "network_error", "exit_status_missing")
}

func TestAttachReportsTruncatedPreAttachBacklog(t *testing.T) {
	// The remote prints more than the retained backlog before any client
	// attaches, which a terminal client must be told about rather than silently
	// receiving a stream that starts mid-output.
	// The flood ends with an OSC7 sequence: once the session reports that path,
	// the client has certainly read every preceding byte.
	flood := append(bytes.Repeat([]byte("pre-attach-output|"), 8192), []byte("]7;file://host/flood-done")...)
	hold := make(chan struct{})
	wroteAll := make(chan struct{})
	sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{
		Scripted:       true,
		SendExitStatus: true,
		Writes:         []sshserver.ScriptedWrite{{Data: flood, ChunkSize: 4096}},
		WritesDone:     wroteAll,
		BeforeExit:     hold,
	})
	server, serverID := newSSHBackedTestServer(t, sshSrv)
	created := createAttachSession(t, server, serverID, "")

	// Attach only after the flood has been written and read, so the retained
	// backlog is guaranteed to have overflowed.
	select {
	case <-wroteAll:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not finish writing the flood")
	}
	waitForCurrentDir(t, server, created.ID, "/flood-done", 10*time.Second)

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 20*time.Second)
	defer conn.Close()

	// The notice follows the snapshot and attach confirmation, before any output.
	readInitialAttachFrames(t, reader)
	opcode, payload, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read truncation notice: %v", err)
	}
	if opcode != wsOpcodeText {
		t.Fatalf("opcode = %d, want a text truncation notice", opcode)
	}
	var notice map[string]any
	if err := json.Unmarshal(payload, &notice); err != nil {
		t.Fatalf("decode notice %q: %v", payload, err)
	}
	if notice["type"] != "session.attach.truncated" {
		t.Fatalf("first frame = %v, want session.attach.truncated", notice)
	}

	// Only a bounded amount of the flood is replayed: drain until the backlog
	// stops arriving, then release the shell so the session can end.
	backlog := readBinaryUntilIdle(t, conn, reader, 300*time.Millisecond)
	close(hold)

	if len(backlog) == 0 {
		t.Fatal("no retained backlog was delivered")
	}
	if len(backlog) >= len(flood) {
		t.Fatalf("delivered %d of %d bytes, want a bounded replay", len(backlog), len(flood))
	}
	if !bytes.HasSuffix(flood, backlog) {
		t.Fatal("the retained backlog is not the newest output")
	}
}

// waitForCurrentDir polls until the session reports the given working directory,
// which the server observes while reading the session's output.
func waitForCurrentDir(t *testing.T, server *Server, id string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if getAttachSession(t, server, id).CurrentDir == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session did not report current dir %q within %s", want, timeout)
}

// readBinaryUntilIdle reads binary frames until none arrives within idle, which
// is how a bounded replay can be collected without waiting for the connection to
// end.
func readBinaryUntilIdle(t *testing.T, conn net.Conn, reader *bufio.Reader, idle time.Duration) []byte {
	t.Helper()
	var collected bytes.Buffer
	for {
		if err := conn.SetReadDeadline(time.Now().Add(idle)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		opcode, payload, err := readServerFrame(reader)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return collected.Bytes()
			}
			return collected.Bytes()
		}
		switch opcode {
		case wsOpcodeBinary:
			collected.Write(payload)
		case wsOpcodeClose:
			return collected.Bytes()
		}
	}
}

// waitForClosedAttach reads until the server ends the attachment, and fails if it
// neither sends a closing frame nor closes the connection within the deadline.
func waitForClosedAttach(t *testing.T, conn net.Conn, reader *bufio.Reader, timeout time.Duration) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for {
		if _, _, err := readServerFrame(reader); err != nil {
			// A read timeout means the server kept the attachment alive.
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				t.Fatal("the detached attachment was left running")
			}
			return
		}
	}
}

// TestReviewControlDetachEndsOldAttachment is the R09 requirement on the WebSocket
// path: an explicit detach must end the old connection, and a new attachment must
// then own the session alone rather than racing the old one for output.
func TestReviewControlDetachEndsOldAttachment(t *testing.T) {
	phaseOne := bytes.Repeat([]byte("first-phase|"), 16)
	phaseTwo := bytes.Repeat([]byte("second-phase|"), 24)
	gateOne := make(chan struct{})
	gateTwo := make(chan struct{})

	sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{
		Scripted:       true,
		SendExitStatus: true,
		Writes: []sshserver.ScriptedWrite{
			{Data: phaseOne, ChunkSize: 16, Gate: gateOne},
			{Data: phaseTwo, ChunkSize: 16, Gate: gateTwo},
		},
	})
	server, serverID := newSSHBackedTestServer(t, sshSrv)
	created := createAttachSession(t, server, serverID, "")

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	firstConn, firstReader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 20*time.Second)
	defer firstConn.Close()
	readInitialAttachFrames(t, firstReader)
	close(gateOne)

	firstBytes := readBinaryBytes(t, firstReader, len(phaseOne), 5*time.Second)
	if !bytes.Equal(firstBytes, phaseOne) {
		t.Fatalf("first attachment read %q, want %q", firstBytes, phaseOne)
	}

	// Detach through the control channel: the server must revoke the attachment
	// and end the connection, not merely clear the attached flag.
	if _, err := firstConn.Write(maskedClientFrame(wsOpcodeText, []byte(`{"type":"detach"}`))); err != nil {
		t.Fatalf("send detach: %v", err)
	}
	waitForClosedAttach(t, firstConn, firstReader, 5*time.Second)
	waitForAttachState(t, server, created.ID, 5*time.Second, "detached")

	// The next client owns the session alone: it sees the second phase only.
	secondConn, secondReader := openTestWebSocketWithDeadline(t, ts, "/v1/sessions/"+created.ID+"/attach", 20*time.Second)
	defer secondConn.Close()
	readInitialAttachFrames(t, secondReader)
	close(gateTwo)

	secondBytes := readBinaryBytes(t, secondReader, len(phaseTwo), 5*time.Second)
	if !bytes.Equal(secondBytes, phaseTwo) {
		t.Fatalf("second attachment read %q\nwant                  %q", secondBytes, phaseTwo)
	}
}

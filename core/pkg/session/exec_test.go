package session

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"

	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func awaitExec(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("exec worker did not finish")
	}
}

func memoryExec(t *testing.T, client *ssh.Client, ctx context.Context) (Exec, <-chan struct{}) {
	t.Helper()
	sess, err, opened := openExecSession(ctx, client)
	awaitExec(t, opened)
	if err != nil {
		t.Fatal(err)
	}
	return executeSSH(ctx, sess, Exec{State: "running", Command: "controlled-command"}, config.RuntimeConfig{})
}

// Setup assertions run in the test goroutine; the background worker only
// executes and returns its result, so a setup failure cannot become a timeout.
func startMemoryExec(t *testing.T, client *ssh.Client, ctx context.Context) <-chan Exec {
	t.Helper()
	sess, err, opened := openExecSession(ctx, client)
	awaitExec(t, opened)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Exec, 1)
	go func() {
		result, settled := executeSSH(ctx, sess, Exec{State: "running", Command: "controlled-command"}, config.RuntimeConfig{})
		<-settled
		done <- result
	}()
	return done
}

func TestExecStreamsAndExitStatus(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(string(rune('0'+code)), func(t *testing.T) {
			srv, client := sshserver.NewMemory(t, sshserver.Config{User: testSSHUser, Password: testSSHPassword})
			remoteDone := make(chan struct{})
			srv.SetExecBehavior(sshserver.ExecBehavior{Scripted: true, SendExitStatus: true, ExitCode: code, Done: remoteDone,
				Writes: []sshserver.ScriptedWrite{{Data: []byte("out\x00中文\r\n")}, {Data: []byte("err\n"), Stderr: true}}})
			result, settled := memoryExec(t, client, context.Background())
			awaitExec(t, settled)
			awaitExec(t, remoteDone)
			if result.State != "completed" || result.ExitCode != code || result.Stdout != "out\x00中文\r\n" || result.Stderr != "err\n" || result.FrameworkCode != "" || result.CleanupError != "" {
				t.Fatalf("result: %+v", result)
			}
		})
	}
}

func TestExecTruncatesEachStreamAtBoundary(t *testing.T) {
	for _, size := range []int{maxExecOutput, maxExecOutput + 1, maxExecOutput * 4} {
		srv, client := sshserver.NewMemory(t, sshserver.Config{User: testSSHUser, Password: testSSHPassword})
		stdout := bytes.Repeat([]byte("a"), size)
		stderr := bytes.Repeat([]byte("b"), size)
		srv.SetExecBehavior(sshserver.ExecBehavior{Scripted: true, SendExitStatus: true,
			Writes: []sshserver.ScriptedWrite{{Data: stdout, ChunkSize: 8191}, {Data: stderr, Stderr: true, ChunkSize: 8191}}})
		result, settled := memoryExec(t, client, context.Background())
		awaitExec(t, settled)
		if result.State != "completed" || result.Truncated != (size > maxExecOutput) || result.Stdout != string(stdout[:maxExecOutput]) || result.Stderr != string(stderr[:maxExecOutput]) {
			t.Fatalf("size=%d state=%s lengths=%d/%d truncated=%v", size, result.State, len(result.Stdout), len(result.Stderr), result.Truncated)
		}
	}
	w := &limitedWriter{limit: 3}
	if n, err := w.Write([]byte("abcdef")); n != 6 || err != nil {
		t.Fatalf("writer returned %d %v", n, err)
	}
	got, truncated := w.snapshot()
	if got != "abc" || !truncated {
		t.Fatalf("writer snapshot=%q %v", got, truncated)
	}
}

func TestExecCancelWaitsForRunAndKeepsSharedChannels(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		srv, client := sshserver.NewMemory(t, sshserver.Config{User: testSSHUser, Password: testSSHPassword, SFTPRoot: t.TempDir()})
		// Real SFTP and shell channels remain open throughout exec cancellation.
		files, err := pkgsftp.NewClient(client)
		if err != nil {
			t.Fatal(err)
		}
		defer files.Close()
		pty, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer pty.Close()
		defer client.Close()
		if _, err := pty.StdinPipe(); err != nil {
			t.Fatal(err)
		}
		if err := pty.Shell(); err != nil {
			t.Fatal(err)
		}
		started, remoteDone := make(chan struct{}), make(chan struct{})
		srv.SetExecBehavior(sshserver.ExecBehavior{Scripted: true, IgnoreSignal: true, Started: started, Done: remoteDone,
			BeforeExit: make(chan struct{}), Writes: []sshserver.ScriptedWrite{{Data: []byte("partial")}}})
		ctx, cancel := context.WithCancel(context.Background())
		if deadline {
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
		}
		done := startMemoryExec(t, client, ctx)
		awaitExec(t, started)
		if !deadline {
			cancel()
		}
		var result Exec
		select {
		case result = <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("canceled execution did not finish")
		}
		cancel()
		awaitExec(t, remoteDone)
		want := "canceled"
		if deadline {
			want = "timeout"
		}
		if result.FrameworkCode != want || result.State != "failed" || result.ExitCode != -1 || result.CleanupError != "" {
			t.Fatalf("result=%+v", result)
		}
		if len(srv.Signals()) == 0 {
			t.Fatal("signal was never attempted")
		}
		if _, err := files.ReadDir("."); err != nil {
			t.Fatalf("shared SFTP: %v", err)
		}
		if err := pty.WindowChange(40, 120); err != nil {
			t.Fatalf("shared shell: %v", err)
		}
		srv.SetExecBehavior(sshserver.ExecBehavior{})
		second, settled := memoryExec(t, client, context.Background())
		awaitExec(t, settled)
		if second.State != "completed" {
			t.Fatalf("shared connection: %+v", second)
		}
	}
}

func TestExecCancelWhileWritingOutput(t *testing.T) {
	srv, client := sshserver.NewMemory(t, sshserver.Config{User: testSSHUser, Password: testSSHPassword})
	for i := 0; i < 50; i++ {
		started, remoteDone := make(chan struct{}), make(chan struct{})
		firstChunk := make(chan struct{})
		srv.SetExecBehavior(sshserver.ExecBehavior{Scripted: true, Started: started, Done: remoteDone, BeforeExit: make(chan struct{}),
			Writes: []sshserver.ScriptedWrite{{Data: bytes.Repeat([]byte("o"), maxExecOutput*2), ChunkSize: 113, FirstChunkSent: firstChunk}, {Data: bytes.Repeat([]byte("e"), maxExecOutput*2), Stderr: true}}})
		ctx, cancel := context.WithCancel(context.Background())
		done := startMemoryExec(t, client, ctx)
		awaitExec(t, started)
		awaitExec(t, firstChunk)
		cancel()
		select {
		case result := <-done:
			if result.FrameworkCode != "canceled" || result.CleanupError != "" || len(result.Stdout) > maxExecOutput || len(result.Stderr) > maxExecOutput {
				t.Fatalf("iteration %d: %+v", i, result)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancel stalled")
		}
		awaitExec(t, remoteDone)
	}
}

func TestExecMissingExitStatus(t *testing.T) {
	srv, client := sshserver.NewMemory(t, sshserver.Config{User: testSSHUser, Password: testSSHPassword})
	srv.SetExecBehavior(sshserver.ExecBehavior{Scripted: true})
	result, settled := memoryExec(t, client, context.Background())
	awaitExec(t, settled)
	if result.FrameworkCode != "exit_status_missing" || result.State != "failed" || result.ExitCode != -1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecFixtureGlobalGates(t *testing.T) {
	srv, client := sshserver.NewMemory(t, sshserver.Config{User: testSSHUser, Password: testSSHPassword})
	beforeExec, beforeExit := make(chan struct{}), make(chan struct{})
	var releaseExec, releaseExit sync.Once
	t.Cleanup(func() { releaseExec.Do(func() { close(beforeExec) }); releaseExit.Do(func() { close(beforeExit) }) })
	srv.SetExecBarrier(beforeExec)
	srv.SetExitBarrier(beforeExit)
	started := make(chan struct{})
	srv.SetExecBehavior(sshserver.ExecBehavior{Scripted: true, SendExitStatus: true, ExitCode: 7, Started: started})
	done := startMemoryExec(t, client, context.Background())
	select {
	case <-started:
		t.Fatal("exec started before its gate opened")
	default:
	}
	releaseExec.Do(func() { close(beforeExec) })
	awaitExec(t, started)
	select {
	case <-done:
		t.Fatal("exec exited before its gate opened")
	default:
	}
	releaseExit.Do(func() { close(beforeExit) })
	select {
	case result := <-done:
		if result.State != "completed" || result.ExitCode != 7 {
			t.Fatalf("result=%+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("released exec did not finish")
	}
}

func TestExecContextValidationAndShutdown(t *testing.T) {
	svc := NewService()
	svc.UseLocalTestBackend()
	for _, req := range []ExecRequest{{}, {ServerRef: "s"}, {ServerRef: "s", Command: "c", TimeoutMS: -1}, {ServerRef: "s", Command: "c", TimeoutMS: math.MaxInt64}, {ServerRef: "s", Command: "c", HostKeyPolicy: "invalid"}} {
		if _, err := svc.ExecContext(context.Background(), req); !errors.Is(err, ErrValidation) {
			t.Fatalf("validation=%v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := svc.ExecContext(ctx, ExecRequest{ServerRef: "s", Command: "c"})
	if err != nil || result.FrameworkCode != "canceled" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if err := svc.ShutdownExec(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Exec(ExecRequest{ServerRef: "s", Command: "c"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("shutdown accepted exec: %v", err)
	}
}

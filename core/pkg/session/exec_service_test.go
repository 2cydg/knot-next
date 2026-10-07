package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/sshpool"
)

// These TCP tests cover the pool/service boundary. Memory protocol tests in
// exec_test.go separately exercise execution without depending on sockets.
func TestExecServiceCancellationReleasesReference(t *testing.T) {
	srv := startSSHServer(t, sshserver.ShellBehavior{})
	service, serverID, _ := newSSHService(t, srv)
	started, remoteDone := make(chan struct{}), make(chan struct{})
	srv.SetExecBehavior(sshserver.ExecBehavior{Scripted: true, Started: started, Done: remoteDone, BeforeExit: make(chan struct{})})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Exec, 1)
	go func() {
		result, _ := service.ExecContext(ctx, ExecRequest{ServerRef: serverID, Command: "wait", HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
		done <- result
	}()
	awaitExec(t, started)
	cancel()
	select {
	case result := <-done:
		if result.FrameworkCode != "canceled" || result.CleanupError != "" {
			t.Fatalf("result=%+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exec caller still running")
	}
	awaitExec(t, remoteDone)
	service.mu.RLock()
	active := len(service.execActive)
	service.mu.RUnlock()
	if active != 0 {
		t.Fatalf("active exec workers=%d", active)
	}
	for _, entry := range service.pool.Stats() {
		if entry.RefCount != 0 {
			t.Fatalf("pool ref=%+v", entry)
		}
	}
	srv.SetExecBehavior(sshserver.ExecBehavior{})
	result, err := service.Exec(ExecRequest{ServerRef: serverID, Command: "next", HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
	if err != nil || result.State != "completed" {
		t.Fatalf("reuse=%+v %v", result, err)
	}
}

func TestExecServiceShutdownStopsUnlimitedCommand(t *testing.T) {
	srv := startSSHServer(t, sshserver.ShellBehavior{})
	service, serverID, _ := newSSHService(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.UseContext(ctx)
	started, remoteDone := make(chan struct{}), make(chan struct{})
	srv.SetExecBehavior(sshserver.ExecBehavior{Scripted: true, Started: started, Done: remoteDone, BeforeExit: make(chan struct{})})
	done := make(chan Exec, 1)
	go func() {
		result, _ := service.Exec(ExecRequest{ServerRef: serverID, Command: "wait", HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
		done <- result
	}()
	awaitExec(t, started)
	cancel()
	budget, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := service.ShutdownExec(budget); err != nil {
		t.Fatal(err)
	}
	result := <-done
	awaitExec(t, remoteDone)
	if result.FrameworkCode != "service_shutdown" || result.CleanupError != "" {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecConnectionDeadlineDoesNotStartCommand(t *testing.T) {
	srv := startSSHServer(t, sshserver.ShellBehavior{})
	barrier := make(chan struct{})
	srv.SetHandshakeBarrier(barrier)
	t.Cleanup(func() { close(barrier) })
	service, serverID, _ := newSSHService(t, srv)
	result, err := service.Exec(ExecRequest{ServerRef: serverID, Command: "never", TimeoutMS: 100, HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
	if err != nil || result.FrameworkCode != "timeout" || len(srv.ExecCommands()) != 0 {
		t.Fatalf("result=%+v err=%v commands=%v", result, err, srv.ExecCommands())
	}
}

func TestExecTrustAndAuthenticationFailures(t *testing.T) {
	for _, scenario := range []string{"unknown_host", "wrong_password", "missing_password"} {
		t.Run(scenario, func(t *testing.T) {
			srv := startSSHServer(t, sshserver.ShellBehavior{})
			service, serverID, cfg := newSSHService(t, srv)
			profile := cfg.runtime.Servers[serverID]
			profile.KnownHostsPath = filepath.Join(t.TempDir(), "known_hosts")
			policy := sshpool.HostKeyPolicyInsecureSkip
			want := "authentication_failed"
			switch scenario {
			case "unknown_host":
				policy = "ask"
				want = "host_key_verification_failed"
			case "wrong_password":
				profile.Password = "wrong"
			case "missing_password":
				profile.Password = ""
			}
			cfg.runtime.Servers[serverID] = profile
			result, err := service.Exec(ExecRequest{ServerRef: serverID, Command: "never", HostKeyPolicy: policy})
			if err != nil || result.FrameworkCode != want || result.ExitCode != -1 || result.State != "failed" || len(srv.ExecCommands()) != 0 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

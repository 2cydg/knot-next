package session

import (
	"context"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/sshpool"
)

func TestKeyedExecRealShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	srv := startSSHServer(t, sshserver.ShellBehavior{})
	srv.SetExecBehavior(sshserver.ExecBehavior{RunLocalShell: true})
	service, id, _ := newSSHService(t, srv)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.ShutdownExec(ctx); err != nil {
			t.Error(err)
		}
	})
	run := func(key, command string, timeout int64) Exec {
		t.Helper()
		res, err := service.Exec(ExecRequest{ServerRef: id, SessionKey: key, Command: command, TimeoutMS: timeout, HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := run("one", "cd /; export KNOT_TEST_VALUE=kept", 0); r.State != "completed" || r.ExitCode != 0 {
		t.Fatalf("first: %+v", r)
	}
	if r := run("one", "pwd; printf %s \"$KNOT_TEST_VALUE\"; printf error >&2; false", 0); r.State != "completed" || r.Stdout != "/\nkept" || r.Stderr != "error" || r.ExitCode != 1 {
		t.Fatalf("second: %+v", r)
	}
	if r := run("two", "printf %s \"${KNOT_TEST_VALUE-unset}\"", 0); r.Stdout != "unset" {
		t.Fatalf("isolation: %+v", r)
	}
	var wg sync.WaitGroup
	results := make(chan string, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := service.Exec(ExecRequest{ServerRef: id, SessionKey: "one", Command: `n=$((${n:-0}+1)); printf %s "$n"`, TimeoutMS: 2000, HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
			if err != nil || r.State != "completed" {
				results <- "error"
				return
			}
			results <- r.Stdout
		}()
	}
	wg.Wait()
	close(results)
	var numbers []string
	for value := range results {
		numbers = append(numbers, value)
	}
	sort.Strings(numbers)
	if strings.Join(numbers, ",") != "1,2,3,4,5" {
		t.Fatalf("concurrent commands: %v", numbers)
	}
	if r := run("one", "sleep 0.3", 30); r.FrameworkCode != "timeout" {
		t.Fatalf("timeout: %+v", r)
	}
	if r := run("one", "printf %s \"${KNOT_TEST_VALUE-unset}\"", 2000); r.Stdout != "unset" {
		t.Fatalf("recreated: %+v", r)
	}
	commands := srv.ExecCommands()
	if strings.Count(strings.Join(commands, ","), "sh -s") != 3 {
		t.Fatalf("channels: %q", commands)
	}
}

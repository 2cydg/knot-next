//go:build !windows

package sshpool

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
)

// Temporary Unix Agent uses real Agent framing and tracks every accepted socket.
func temporaryAgent(t *testing.T, keyring agent.Agent) (string, func() int) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "agent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	active := map[net.Conn]bool{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			active[conn] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = agent.ServeAgent(keyring, conn)
				mu.Lock()
				delete(active, conn)
				mu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		for conn := range active {
			_ = conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return socket, func() int { mu.Lock(); defer mu.Unlock(); return len(active) }
}
func TestAgentAuthenticatesAndClosesTemporaryConnection(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.NewSignerFromKey(private)
	ring := agent.NewKeyring()
	if err = ring.Add(agent.AddedKey{PrivateKey: private}); err != nil {
		t.Fatal(err)
	}
	socket, active := temporaryAgent(t, ring)
	remote := sshserver.New(t, sshserver.Config{User: "agent-user", PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() != "agent-user" || !bytes.Equal(key.Marshal(), signer.PublicKey().Marshal()) {
			return nil, errors.New("refused")
		}
		return nil, nil
	}})
	pool := NewPool()
	defer pool.CloseAll()
	server := config.ServerProfile{ID: "target", Alias: "target", Host: remote.Host(), Port: remote.Port(), User: "agent-user", AuthMethod: config.AuthMethodAgent}
	cfg := config.RuntimeConfig{Servers: map[string]config.ServerProfile{server.ID: server}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lease, err := pool.AcquireClientContext(ctx, server, cfg, nil, DialOptions{AgentSocket: socket, HostKeyPolicy: HostKeyPolicyInsecureSkip})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	deadline := time.Now().Add(time.Second)
	for active() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("authentication Agent connection leaked")
		}
		time.Sleep(time.Millisecond)
	}
	session, err := lease.Client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
}

type refusingAgent struct{ agent.Agent }

func (r refusingAgent) Sign(ssh.PublicKey, []byte) (*ssh.Signature, error) {
	return nil, errors.New("sign refused")
}
func TestAgentEmptyRefusedAndCanceled(t *testing.T) {
	for _, test := range []string{"empty", "refused", "missing", "canceled"} {
		t.Run(test, func(t *testing.T) {
			ring := agent.NewKeyring()
			var keyring agent.Agent = ring
			if test == "refused" {
				_, private, _ := ed25519.GenerateKey(rand.Reader)
				_ = ring.Add(agent.AddedKey{PrivateKey: private})
				keyring = refusingAgent{ring}
			}
			socket, _ := temporaryAgent(t, keyring)
			if test == "missing" {
				socket = filepath.Join(t.TempDir(), "missing.sock")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if test == "canceled" {
				cancel()
			}
			server := config.ServerProfile{AuthMethod: config.AuthMethodAgent}
			methods, closer, err := authMethodsContext(ctx, server, config.RuntimeConfig{}, DialOptions{AgentSocket: socket})
			if closer != nil {
				defer closer.Close()
			}
			if test != "refused" {
				if !IsAuthError(err) {
					t.Fatalf("expected AuthError, got %v", err)
				}
				return
			}
			if err != nil || len(methods) == 0 {
				t.Fatal(err)
			}
			remote := sshserver.New(t, sshserver.Config{User: "agent", PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }})
			server = config.ServerProfile{ID: "refused", Host: remote.Host(), Port: remote.Port(), User: "agent", AuthMethod: config.AuthMethodAgent}
			pool := NewPool()
			defer pool.CloseAll()
			_, err = pool.AcquireClientContext(ctx, server, config.RuntimeConfig{Servers: map[string]config.ServerProfile{server.ID: server}}, nil, DialOptions{AgentSocket: socket, HostKeyPolicy: HostKeyPolicyInsecureSkip})
			if !IsAuthError(err) {
				t.Fatalf("sign refusal did not become AuthError: %v", err)
			}
		})
	}
}
func TestAgentProtocolTimeoutAndDisconnect(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "disconnect"}[disconnect], func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "stalled.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				if !disconnect {
					_, _ = io.Copy(io.Discard, conn)
				}
			}()
			started := time.Now()
			_, _, err = authMethodsContext(context.Background(), config.ServerProfile{AuthMethod: config.AuthMethodAgent}, config.RuntimeConfig{}, DialOptions{AgentSocket: socket, Timeout: 50 * time.Millisecond})
			if !IsAuthError(err) || time.Since(started) > time.Second {
				t.Fatalf("unbounded or wrong error: %v", err)
			}
			<-done
		})
	}
}
func TestForwardedAgentSignsAndSharedSessionsReuseHandler(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(private)
	ring := agent.NewKeyring()
	_ = ring.Add(agent.AddedKey{PrivateKey: private})
	socket, active := temporaryAgent(t, ring)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	remoteConn := make(chan *ssh.ServerConn, 1)
	remoteDone := make(chan struct{})
	go func() {
		defer close(remoteDone)
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		cfg := &ssh.ServerConfig{NoClientAuth: true}
		cfg.AddHostKey(signer)
		conn, chans, reqs, err := ssh.NewServerConn(raw, cfg)
		if err != nil {
			return
		}
		defer conn.Close()
		remoteConn <- conn
		go ssh.DiscardRequests(reqs)
		for incoming := range chans {
			channel, requests, err := incoming.Accept()
			if err != nil {
				continue
			}
			go func() {
				defer channel.Close()
				for req := range requests {
					_ = req.Reply(req.Type == "auth-agent-req@openssh.com", nil)
				}
			}()
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	client, err := ssh.Dial("tcp", addr.String(), &ssh.ClientConfig{User: "test", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	pool := NewPool()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sessions := make([]*ssh.Session, 2)
	for i := range sessions {
		sessions[i], err = client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if err = pool.ForwardAgent(ctx, client, sessions[i], socket, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	_ = sessions[0].Close()
	conn := <-remoteConn
	channel, requests, err := conn.OpenChannel("auth-agent@openssh.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(requests)
	agentClient := agent.NewClient(channel)
	keys, err := agentClient.List()
	if err != nil || len(keys) != 1 {
		t.Fatalf("forwarded list: %v %v", keys, err)
	}
	public, err := ssh.ParsePublicKey(keys[0].Blob)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("actual forwarded signing challenge")
	signature, err := agentClient.Sign(public, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = public.Verify(payload, signature); err != nil {
		t.Fatal("invalid forwarded signature", err)
	}
	_ = channel.Close()
	_ = sessions[1].Close()
	_ = client.Close()
	if err = pool.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	<-remoteDone
	deadline := time.Now().Add(time.Second)
	for active() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("forwarded Agent connection leaked")
		}
		time.Sleep(time.Millisecond)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.forwardAgents) != 0 {
		t.Fatal("forwarding handler retained client")
	}
}

func TestAgentCapabilityReportsEnvironment(t *testing.T) {
	ring := agent.NewKeyring()
	socket, _ := temporaryAgent(t, ring)
	t.Setenv("SSH_AUTH_SOCK", socket)
	available, reason := AgentCapability(context.Background())
	if available || reason != "agent_empty" {
		t.Fatalf("empty: %v %s", available, reason)
	}
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	_ = ring.Add(agent.AddedKey{PrivateKey: private})
	available, reason = AgentCapability(context.Background())
	if !available || reason != "" {
		t.Fatalf("available: %v %s", available, reason)
	}
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(t.TempDir(), "missing.sock"))
	available, reason = AgentCapability(context.Background())
	if available || reason != "agent_unavailable" {
		t.Fatalf("missing: %v %s", available, reason)
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	if _, err := openAgent(context.Background(), "", time.Second); err == nil {
		t.Fatal("missing default Agent accepted")
	}
}
func TestForwardingRemoteRefusalEndpointConflictAndClosedPool(t *testing.T) {
	ring := agent.NewKeyring()
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	_ = ring.Add(agent.AddedKey{PrivateKey: private})
	socket, _ := temporaryAgent(t, ring)
	_, client := sshserver.NewMemory(t, sshserver.Config{User: "test", Password: "artificial"})
	pool := NewPool()
	defer func() {
		_ = client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = pool.Shutdown(ctx)
	}()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err = pool.ForwardAgent(context.Background(), client, sess, socket, time.Second); !errors.Is(err, ErrAgentForwarding) {
		t.Fatalf("remote refusal not reported: %v", err)
	}
	another, _ := temporaryAgent(t, ring)
	if err = pool.ForwardAgent(context.Background(), client, sess, another, time.Second); !errors.Is(err, ErrAgentForwarding) {
		t.Fatal("accepted conflicting Agent")
	}
	_ = client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = pool.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err = pool.ForwardAgent(context.Background(), client, sess, socket, time.Second); !errors.Is(err, ErrAgentForwarding) {
		t.Fatal("accepted closed pool")
	}
}

func TestAgentSignerSurvivesHostKeyPrompt(t *testing.T) {
	ring := agent.NewKeyring()
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	_ = ring.Add(agent.AddedKey{PrivateKey: private})
	socket, _ := temporaryAgent(t, ring)
	remote := sshserver.New(t, sshserver.Config{User: "agent", PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }})
	pool := NewPool()
	defer pool.CloseAll()
	profile := config.ServerProfile{ID: "agent", Alias: "agent", Host: remote.Host(), Port: remote.Port(), User: "agent", AuthMethod: config.AuthMethodAgent, KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts")}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lease, err := pool.AcquireClientContextWithPrompt(ctx, profile, config.RuntimeConfig{Servers: map[string]config.ServerProfile{profile.ID: profile}}, func(ctx context.Context, _ HostKeyPrompt) bool {
		timer := time.NewTimer(200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return true
		case <-ctx.Done():
			return false
		}
	}, DialOptions{AgentSocket: socket, Timeout: 80 * time.Millisecond})
	if err != nil {
		t.Fatalf("Agent phase expired during host key confirmation: %v", err)
	}
	lease.Release()
}

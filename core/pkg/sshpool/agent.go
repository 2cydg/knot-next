package sshpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var ErrAgentForwarding = errors.New("agent forwarding unavailable")

type agentConn struct {
	net.Conn
	stop    func() bool
	once    sync.Once
	ctx     context.Context
	timeout time.Duration
}

func (c *agentConn) Close() error {
	c.once.Do(func() {
		if c.stop != nil {
			c.stop()
		}
	})
	return c.Conn.Close()
}

func (c *agentConn) deadline() time.Time {
	if c.timeout <= 0 {
		return time.Time{}
	}
	deadline := time.Now().Add(c.timeout)
	if d, ok := c.ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return deadline
}
func (c *agentConn) Read(data []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(c.deadline()); err != nil {
		return 0, err
	}
	return c.Conn.Read(data)
}
func (c *agentConn) Write(data []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(c.deadline()); err != nil {
		return 0, err
	}
	return c.Conn.Write(data)
}

// Each Agent exchange has a phase deadline. Time spent waiting for a user host
// key challenge must not expire a signer connection before signing begins.
// The authentication connection is retained only until SSH handshake completes.
func openAgent(ctx context.Context, socket string, timeout time.Duration) (net.Conn, error) {
	if socket == "" {
		socket = defaultAgentSocket()
	}
	if socket == "" {
		return nil, errors.New("ssh agent endpoint is not configured")
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dialAgentContext(dialCtx, socket)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err = conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	c := &agentConn{Conn: conn, ctx: ctx, timeout: timeout}
	c.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	return c, nil
}

// AgentCapability performs a bounded, read-only public-key listing, never signing.
func AgentCapability(ctx context.Context) (bool, string) {
	conn, err := openAgent(ctx, "", 250*time.Millisecond)
	if err != nil {
		return false, "agent_unavailable"
	}
	defer conn.Close()
	keys, err := agent.NewClient(conn).List()
	if err != nil {
		return false, "agent_unavailable"
	}
	if len(keys) == 0 {
		return false, "agent_empty"
	}
	return true, ""
}

// ForwardAgent registers one handler per client. Each remote Agent channel owns
// a fresh local connection; closing a session cannot tear down other sessions.
func (p *Pool) ForwardAgent(ctx context.Context, client *ssh.Client, session *ssh.Session, socket string, timeout time.Duration) error {
	if socket == "" {
		socket = defaultAgentSocket()
	}
	probe, err := openAgent(ctx, socket, timeout)
	if err != nil {
		return fmt.Errorf("%w: agent_unavailable", ErrAgentForwarding)
	}
	keys, err := agent.NewClient(probe).List()
	_ = probe.Close()
	if err != nil || len(keys) == 0 {
		return fmt.Errorf("%w: agent_has_no_usable_keys", ErrAgentForwarding)
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return fmt.Errorf("%w: pool_closed", ErrAgentForwarding)
	}
	if p.forwardAgents == nil {
		p.forwardAgents = map[*ssh.Client]string{}
	}
	previous, exists := p.forwardAgents[client]
	if exists && previous != socket {
		p.mu.Unlock()
		return fmt.Errorf("%w: agent_endpoint_conflict", ErrAgentForwarding)
	}
	if !exists {
		channels := client.HandleChannelOpen("auth-agent@openssh.com")
		if channels == nil {
			p.mu.Unlock()
			return fmt.Errorf("%w: handler_conflict", ErrAgentForwarding)
		}
		p.forwardAgents[client] = socket
		p.workers.Add()
		go p.serveForwardedAgent(client, socket, channels)
	}
	p.mu.Unlock()
	if err = agent.RequestAgentForwarding(session); err != nil {
		return fmt.Errorf("%w: remote_refused", ErrAgentForwarding)
	}
	return nil
}
func (p *Pool) serveForwardedAgent(client *ssh.Client, socket string, channels <-chan ssh.NewChannel) {
	defer p.workers.Done()
	defer func() { p.mu.Lock(); delete(p.forwardAgents, client); p.mu.Unlock() }()
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = client.Wait(); cancel(); close(done) }()
	defer func() { <-done }()
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	slots := make(chan struct{}, 32)
	for incoming := range channels {
		select {
		case slots <- struct{}{}:
		default:
			_ = incoming.Reject(ssh.ResourceShortage, "agent channel limit")
			continue
		}
		workers.Add(1)
		go func(incoming ssh.NewChannel) {
			defer workers.Done()
			defer func() { <-slots }()
			local, err := openAgent(ctx, socket, 15*time.Second)
			if err != nil {
				_ = incoming.Reject(ssh.ConnectionFailed, "local agent unavailable")
				return
			}
			defer local.Close()
			remote, requests, err := incoming.Accept()
			if err != nil {
				return
			}
			defer remote.Close()
			// Configure the channel owner before either copy starts. Idle forwarded
			// streams end through ctx rather than the authentication phase timer.
			local.(*agentConn).timeout = 0
			_ = local.SetDeadline(time.Time{})
			stop := context.AfterFunc(ctx, func() { _ = remote.Close(); _ = local.Close() })
			defer stop()
			reqDone := make(chan struct{})
			go func() { ssh.DiscardRequests(requests); close(reqDone) }()
			copied := make(chan struct{})
			go func() { _, _ = io.Copy(local, remote); _ = local.Close(); _ = remote.Close(); close(copied) }()
			_, _ = io.Copy(remote, local)
			_ = remote.Close()
			_ = local.Close()
			<-copied
			<-reqDone
		}(incoming)
	}
}

package forward

import (
	"context"
	"io"
	"knot-core/internal/resourcepolicy"
	"knot-core/pkg/sshpool"
	"net"
	"sync"
	"time"
)

const setupTimeout = 15 * time.Second

func (s *Service) run(ctx context.Context, r *resource, lease *sshpool.ClientLease) {
	ctx, work := resourcepolicy.Scope(ctx)
	resourcepolicy.Go(ctx, func() {
		select {
		case <-lease.Done:
			r.cancel()
		case <-ctx.Done():
		}
	})
	stop := resourcepolicy.AfterFunc(ctx, func() { s.closeRuntime(ctx, r) })
	listener, err := s.listen(ctx, r, lease)
	if err == nil {
		s.mu.Lock()
		if ctx.Err() == nil {
			r.listener = listener
			r.BoundAddress = listener.Addr().String()
			r.State = "active"
			r.UpdatedAt = s.policy.Now().UTC()
			s.emitLocked(r, "forward.active")
		} else {
			resourcepolicy.Go(ctx, func() { _ = listener.Close() })
		}
		s.mu.Unlock()
		if ctx.Err() == nil {
			err = s.accept(ctx, r, listener, lease)
		}
	}
	failed := err != nil && ctx.Err() == nil
	r.cancel()
	s.mu.Lock()
	if r.State == "active" || r.State == "starting" {
		r.State = "stopping"
		r.UpdatedAt = s.policy.Now().UTC()
		s.emitLocked(r, "forward.stopping")
	}
	s.mu.Unlock()
	s.closeRuntime(ctx, r)
	stop()
	// Late remote Listen/Dial replies are part of this run's actual ownership.
	// A public stop timeout never releases its transport lease early.
	_ = work.Wait(context.Background())
	lease.Release()
	s.mu.Lock()
	now := s.policy.Now().UTC()
	r.listener = nil
	r.UpdatedAt = now
	r.EndedAt = &now
	if failed && !s.stopped {
		r.State = "failed"
		r.FailureCode = "FORWARD_START_FAILED"
		r.Error = "forward listener failed"
	} else {
		r.State = "stopped"
	}
	// A transport loss is distinct from an explicitly requested stop.
	select {
	case <-lease.Done:
		if r.State != "failed" && !s.stopped {
			r.State = "failed"
			r.FailureCode = "CONNECTION_CLOSED"
			r.Error = "forward connection closed"
		}
	default:
	}
	close(r.done)
	s.emitLocked(r, "forward."+r.State)
	s.pruneLocked()
	s.mu.Unlock()
}
func (s *Service) closeRuntime(ctx context.Context, r *resource) {
	s.mu.Lock()
	listener := r.listener
	r.listener = nil
	conns := make([]net.Conn, 0, len(r.streams))
	for conn := range r.streams {
		conns = append(conns, conn)
		delete(r.streams, conn)
	}
	s.mu.Unlock()
	// Remote listener Close sends cancel-tcpip-forward and can await a peer reply.
	if listener != nil {
		resourcepolicy.Go(ctx, func() { _ = listener.Close() })
	}
	for _, conn := range conns {
		resourcepolicy.Go(ctx, func() { _ = conn.Close() })
	}
}
func (s *Service) listen(ctx context.Context, r *resource, lease *sshpool.ClientLease) (net.Listener, error) {
	if r.Type != "remote" {
		return (&net.ListenConfig{}).Listen(ctx, "tcp", r.Bind)
	}
	stage, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	type result struct {
		listener net.Listener
		err      error
	}
	done := make(chan result, 1)
	resourcepolicy.Go(ctx, func() { listener, err := lease.Client.Listen("tcp", r.Bind); done <- result{listener, err} })
	select {
	case got := <-done:
		return got.listener, got.err
	case <-stage.Done():
		resourcepolicy.Go(ctx, func() {
			if got := <-done; got.listener != nil {
				_ = got.listener.Close()
			}
		})
		return nil, stage.Err()
	}
}
func (s *Service) accept(ctx context.Context, r *resource, listener net.Listener, lease *sshpool.ClientLease) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case s.streamSlots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		s.mu.Lock()
		if ctx.Err() != nil || len(r.streams) >= 128 {
			s.mu.Unlock()
			<-s.streamSlots
			_ = conn.Close()
			continue
		}
		r.streams[conn] = struct{}{}
		s.mu.Unlock()
		resourcepolicy.Go(ctx, func() { defer func() { <-s.streamSlots }(); s.handle(ctx, r, conn, lease) })
	}
}
func (s *Service) handle(ctx context.Context, r *resource, source net.Conn, lease *sshpool.ClientLease) {
	defer func() { _ = source.Close(); s.mu.Lock(); delete(r.streams, source); s.mu.Unlock() }()
	target := r.Target
	if r.Type == "dynamic" {
		_ = source.SetDeadline(time.Now().Add(10 * time.Second))
		available, err := readSocks5Greeting(source)
		if err != nil {
			return
		}
		if !available {
			_, _ = source.Write([]byte{5, 255})
			return
		}
		if err = writeSocks5NoAuth(source); err != nil {
			return
		}
		target, err = readSocks5Request(source)
		if err != nil {
			if code, ok := socks5FailureCode(err); ok {
				_ = writeSocks5Failure(source, code)
			}
			return
		}
	}
	stage, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	var dest net.Conn
	var err error
	if r.Type == "remote" {
		dest, err = (&net.Dialer{}).DialContext(stage, "tcp", target)
	} else {
		dest, err = s.dialSSH(stage, lease, target)
	}
	if err != nil {
		if r.Type == "dynamic" {
			_ = writeSocks5Failure(source, dialFailureCode(err))
		}
		s.flowFailure(r)
		return
	}
	defer dest.Close()
	s.mu.Lock()
	if ctx.Err() != nil || len(r.streams) >= 128 {
		s.mu.Unlock()
		return
	}
	r.streams[dest] = struct{}{}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(r.streams, dest); s.mu.Unlock() }()
	if r.Type == "dynamic" {
		if err = writeSocks5Success(source); err != nil {
			return
		}
		_ = source.SetDeadline(time.Time{})
	}
	bridge(ctx, source, dest)
}
func (s *Service) flowFailure(r *resource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.State == "active" {
		r.UpdatedAt = s.policy.Now().UTC()
		s.emitLocked(r, "forward.flow_failed")
	}
}
func (s *Service) dialSSH(ctx context.Context, lease *sshpool.ClientLease, target string) (net.Conn, error) {
	select {
	case s.dialSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	resourcepolicy.Go(ctx, func() {
		defer func() { <-s.dialSlots }()
		conn, err := lease.Client.Dial("tcp", target)
		done <- result{conn, err}
	})
	select {
	case got := <-done:
		return got.conn, got.err
	case <-ctx.Done():
		resourcepolicy.Go(ctx, func() {
			if got := <-done; got.conn != nil {
				_ = got.conn.Close()
			}
		})
		return nil, ctx.Err()
	}
}
func bridge(ctx context.Context, a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	copyStream := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if closeWrite, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = closeWrite.CloseWrite()
		}
		if err != nil {
			_ = a.Close()
			_ = b.Close()
		}
	}
	go copyStream(a, b)
	go copyStream(b, a)
	stop := resourcepolicy.AfterFunc(ctx, func() { _ = a.Close(); _ = b.Close() })
	defer stop()
	wg.Wait()
}

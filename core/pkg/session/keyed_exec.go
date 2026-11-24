package session

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"knot-core/pkg/config"
	"knot-core/pkg/sshpool"
)

const keyedIdle = 30 * time.Minute
const maxKeyedExecs = 128

type keyedExec struct {
	gate           chan struct{}
	session        *ssh.Session
	lease          *sshpool.ClientLease
	stdin          io.WriteCloser
	stdout, stderr *bufio.Reader
	timer          *time.Timer
	generation     uint64
	identity       []byte
}

func (s *Service) closeKeyedExecs() {
	s.keyedMu.Lock()
	defer s.keyedMu.Unlock()
	for key, slot := range s.keyedExecs {
		delete(s.keyedExecs, key)
		if slot.timer != nil {
			slot.timer.Stop()
		}
		if slot.session != nil {
			_ = slot.session.Close()
		}
		select {
		case <-slot.gate:
			if slot.lease != nil {
				slot.lease.Release()
				slot.lease = nil
			}
			slot.gate <- struct{}{}
		default:
		}
	}
}

func (s *Service) keyedSlot(ctx context.Context, key string) (*keyedExec, error) {
	for {
		s.keyedMu.Lock()
		if s.execCtx.Err() != nil {
			s.keyedMu.Unlock()
			return nil, errExecShutdown
		}
		slot := s.keyedExecs[key]
		if slot == nil {
			if len(s.keyedExecs) >= maxKeyedExecs {
				s.keyedMu.Unlock()
				return nil, fmt.Errorf("keyed exec limit reached")
			}
			slot = &keyedExec{gate: make(chan struct{}, 1)}
			slot.gate <- struct{}{}
			s.keyedExecs[key] = slot
		}
		s.keyedMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-slot.gate:
		}
		s.keyedMu.Lock()
		valid := s.keyedExecs[key] == slot && s.execCtx.Err() == nil
		if valid && slot.timer != nil {
			slot.timer.Stop()
			slot.timer = nil
		}
		if valid {
			slot.generation++
		}
		s.keyedMu.Unlock()
		if valid {
			return slot, nil
		}
		slot.gate <- struct{}{}
		if s.execCtx.Err() != nil {
			return nil, errExecShutdown
		}
	}
}

func (s *Service) discardKeyed(key string, slot *keyedExec) {
	s.keyedMu.Lock()
	if s.keyedExecs[key] == slot {
		delete(s.keyedExecs, key)
	}
	if slot.timer != nil {
		slot.timer.Stop()
		slot.timer = nil
	}
	if slot.session != nil {
		_ = slot.session.Close()
		slot.session = nil
	}
	if slot.lease != nil {
		slot.lease.Release()
		slot.lease = nil
	}
	s.keyedMu.Unlock()
}

func (s *Service) releaseKeyed(key string, slot *keyedExec) {
	s.keyedMu.Lock()
	valid := s.keyedExecs[key] == slot
	if valid {
		generation := slot.generation
		slot.timer = time.AfterFunc(keyedIdle, func() {
			select {
			case <-slot.gate:
				s.keyedMu.Lock()
				expired := s.keyedExecs[key] == slot && slot.generation == generation
				s.keyedMu.Unlock()
				if expired {
					s.discardKeyed(key, slot)
				}
				slot.gate <- struct{}{}
			default:
			}
		})
	}
	s.keyedMu.Unlock()
	if !valid {
		s.discardKeyed(key, slot)
	}
	slot.gate <- struct{}{}
}

func (s *Service) runKeyedExec(ctx context.Context, result Exec, req ExecRequest, server config.ServerProfile, cfg config.RuntimeConfig, pool *sshpool.Pool, dial DialOptions, started func()) (Exec, <-chan struct{}) {
	key := server.ID + "\x00" + req.SessionKey
	slot, err := s.keyedSlot(ctx, key)
	if err != nil {
		return failedExec(result, err, cfg), nil
	}
	opts := sshpool.DialOptions{AgentSocket: dial.AgentSocket, HostKeyPolicy: req.HostKeyPolicy, Timeout: dial.Timeout}
	identity := sshpool.ClientIdentity(server, cfg, opts)
	if slot.session != nil && !bytes.Equal(slot.identity, identity) {
		s.keyedMu.Lock()
		_ = slot.session.Close()
		slot.lease.Release()
		slot.session, slot.lease = nil, nil
		s.keyedMu.Unlock()
	}
	if slot.session == nil {
		lease, err := pool.AcquireClientContext(ctx, server, cfg, nil, opts)
		if err != nil {
			s.discardKeyed(key, slot)
			slot.gate <- struct{}{}
			return failedExec(result, err, cfg), nil
		}
		channel, err, settled := openExecSession(ctx, lease.Client)
		if err != nil {
			s.discardKeyed(key, slot)
			slot.gate <- struct{}{}
			released := make(chan struct{})
			go func() { <-settled; lease.Release(); close(released) }()
			return failedExec(result, err, cfg), released
		}
		stdin, err := channel.StdinPipe()
		var stdout, stderr io.Reader
		if err == nil {
			stdout, err = channel.StdoutPipe()
		}
		if err == nil {
			stderr, err = channel.StderrPipe()
		}
		if err != nil {
			_ = channel.Close()
			lease.Release()
			s.discardKeyed(key, slot)
			slot.gate <- struct{}{}
			return failedExec(result, err, cfg), nil
		}
		// Start the non-interactive shell without allocating a PTY so streams remain separate.
		ready := make(chan error, 1)
		go func() { ready <- channel.Start("sh -s") }()
		select {
		case err = <-ready:
			if err != nil {
				_ = channel.Close()
				lease.Release()
				s.discardKeyed(key, slot)
				slot.gate <- struct{}{}
				return failedExec(result, err, cfg), nil
			}
		case <-ctx.Done():
			_ = channel.Close()
			s.discardKeyed(key, slot)
			slot.gate <- struct{}{}
			done := make(chan struct{})
			go func() { <-ready; lease.Release(); close(done) }()
			return failedExec(result, context.Cause(ctx), cfg), done
		}
		s.keyedMu.Lock()
		if s.keyedExecs[key] != slot || s.execCtx.Err() != nil {
			s.keyedMu.Unlock()
			_ = channel.Close()
			lease.Release()
			slot.gate <- struct{}{}
			return failedExec(result, errExecShutdown, cfg), nil
		}
		slot.session, slot.lease, slot.stdin = channel, lease, stdin
		slot.identity = identity
		slot.stdout, slot.stderr = bufio.NewReader(stdout), bufio.NewReader(stderr)
		s.keyedMu.Unlock()
	}
	started()
	var tokenBytes [16]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		s.discardKeyed(key, slot)
		slot.gate <- struct{}{}
		return failedExec(result, err, cfg), nil
	}
	marker := "__KNOT_" + hex.EncodeToString(tokenBytes[:]) + "__"
	// eval runs in the persistent shell, so cd, exports and functions survive.
	quoted := "'" + strings.ReplaceAll(req.Command, "'", "'\\''") + "'"
	script := "eval " + quoted + "\n__knot_status=$?\ncommand printf '" + marker + "%03d\\n' \"$__knot_status\"\ncommand printf '" + marker + "\\n' >&2\n"
	type reply struct {
		out, errout string
		code        int
		truncated   bool
		err         error
	}
	finished := make(chan reply, 1)
	go func() {
		var r reply
		if _, r.err = io.WriteString(slot.stdin, script); r.err == nil {
			type stream struct {
				value     string
				truncated bool
				err       error
			}
			out, errout := make(chan stream, 1), make(chan stream, 1)
			go func() { v, t, e := readKeyedStream(slot.stdout, marker, true); out <- stream{v, t, e} }()
			go func() { v, t, e := readKeyedStream(slot.stderr, marker, false); errout <- stream{v, t, e} }()
			a, b := <-out, <-errout
			r.out, r.errout, r.truncated = a.value, b.value, a.truncated || b.truncated
			r.err = a.err
			if r.err == nil {
				r.err = b.err
			}
			if r.err == nil {
				r.code, r.err = strconv.Atoi(r.out[len(r.out)-3:])
				r.out = r.out[:len(r.out)-3]
			}
		}
		finished <- r
	}()
	select {
	case r := <-finished:
		result.Stdout, result.Stderr, result.Truncated = r.out, r.errout, r.truncated
		if r.err != nil {
			s.discardKeyed(key, slot)
			slot.gate <- struct{}{}
			return failedExec(result, r.err, cfg), nil
		}
		result.State, result.ExitCode, result.CompletedAt = "completed", r.code, time.Now().UTC()
		s.releaseKeyed(key, slot)
		return result, nil
	case <-ctx.Done():
		s.keyedMu.Lock()
		if slot.session != nil {
			_ = slot.session.Close()
		}
		s.keyedMu.Unlock()
		done := make(chan struct{})
		go func() { <-finished; s.discardKeyed(key, slot); slot.gate <- struct{}{}; close(done) }()
		return failedExec(result, context.Cause(ctx), cfg), done
	}
}

// readKeyedStream finds a random marker without assuming output is line based.
func readKeyedStream(r *bufio.Reader, marker string, status bool) (string, bool, error) {
	var output bytes.Buffer
	pending := make([]byte, 0, len(marker))
	truncated := false
	appendByte := func(b byte) {
		if output.Len() < maxExecOutput {
			output.WriteByte(b)
		} else {
			truncated = true
		}
	}
	for {
		b, err := r.ReadByte()
		if err != nil {
			return output.String(), truncated, err
		}
		pending = append(pending, b)
		for len(pending) > 0 && !strings.HasPrefix(marker, string(pending)) {
			appendByte(pending[0])
			pending = pending[1:]
		}
		if string(pending) == marker {
			break
		}
	}
	if status {
		var code [4]byte
		if _, err := io.ReadFull(r, code[:]); err != nil {
			return output.String(), truncated, err
		}
		if code[3] != '\n' || code[0] < '0' || code[0] > '9' || code[1] < '0' || code[1] > '9' || code[2] < '0' || code[2] > '9' {
			return output.String(), truncated, fmt.Errorf("invalid shell status")
		}
		return output.String() + string(code[:3]), truncated, nil
	}
	b, err := r.ReadByte()
	if err != nil {
		return output.String(), truncated, err
	}
	if b != '\n' {
		return output.String(), truncated, fmt.Errorf("invalid shell marker")
	}
	return output.String(), truncated, nil
}

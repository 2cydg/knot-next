// Package directssh connects aliases using console prompts and a native SSH
// terminal, without creating a Bubble Tea program or a management model.
package directssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"knot-cli/internal/connection"
	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/terminal"
)

func Run(ctx context.Context, ref string, tr i18n.Translator, input io.Reader, output, diagnostics io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if ctx.Err() != nil {
		return entry.ExitCanceled
	}
	_ = terminal.SetTitle(output, "Knot")
	defer func() { _ = terminal.SetTitle(output, "Knot") }()
	fail := func(err error) int {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return entry.ExitCanceled
		}
		fmt.Fprintln(diagnostics, tr.ProtocolError(display(coreclient.Code(err))))
		return entry.ExitFailure
	}
	path, err := coreclient.RuntimeFile()
	if err != nil {
		return fail(err)
	}
	loadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	snapshot, err := coreclient.Load(loadCtx, path)
	cancel()
	if err != nil {
		return fail(err)
	}
	defer snapshot.Client.Close()
	resolveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	server, err := snapshot.Client.Resolve(resolveCtx, ref)
	cancel()
	if err != nil {
		return fail(err)
	}
	in, _ := input.(*os.File)
	out, _ := output.(*os.File)
	rows, cols := 24, 80
	if out != nil {
		rows, cols = terminal.Size(out)
	}
	term := os.Getenv("TERM")
	if term == "" || term == "dumb" {
		term = "xterm-256color"
	}
	request := coreclient.SessionRequest{ServerRef: server.ID, Term: term, Rows: rows, Cols: cols, ForwardAgent: snapshot.Available("agent_forwarding"), HostKeyPolicy: "ask", AllowAuthRetry: false}
	fmt.Fprintln(diagnostics, tr.Text("ssh.direct_connecting", map[string]any{"Alias": display(server.Alias)}))
	attempt, session, err := connect(ctx, snapshot, request, func(c coreclient.Challenge) (bool, error) {
		return confirmHost(ctx, c, tr, input, diagnostics)
	})
	if err != nil {
		return fail(err)
	}
	defer func() {
		for _, code := range attempt.Finish() {
			fmt.Fprintln(diagnostics, tr.Text("ui.warning", map[string]any{"Code": display(code)}))
		}
	}()
	title := session.Alias
	if title == "" {
		title = server.Alias
	}
	_ = terminal.SetTitle(output, title)
	result := terminal.Run(ctx, snapshot.Client, snapshot.Runtime.InstanceID, session.ID, in, out)
	if ctx.Err() != nil || (result.Err != nil && !result.Detached) {
		attempt.Close()
	}
	if ctx.Err() != nil {
		return entry.ExitCanceled
	}
	if result.Err != nil {
		fail(result.Err)
	}
	if result.Detached {
		fmt.Fprintln(diagnostics, tr.Text("ssh.detached", map[string]any{"ID": display(session.ID)}))
	}
	if result.Truncated {
		fmt.Fprintln(diagnostics, tr.Message("ssh.truncated"))
	}
	return result.Code()
}

func connect(ctx context.Context, snapshot coreclient.Snapshot, request coreclient.SessionRequest, confirm func(coreclient.Challenge) (bool, error)) (*connection.Attempt, coreclient.Session, error) {
	for {
		attempt := connection.New(ctx, snapshot.Client, snapshot.Runtime.InstanceID)
		session, err := wait(ctx, attempt, request, confirm)
		if err == nil {
			return attempt, session, nil
		}
		attempt.Close()
		if request.ForwardAgent && coreclient.Code(err) == "agent_forwarding_unavailable" && ctx.Err() == nil {
			request.ForwardAgent = false
			continue
		}
		return nil, coreclient.Session{}, err
	}
}

func wait(ctx context.Context, attempt *connection.Attempt, request coreclient.SessionRequest, confirm func(coreclient.Challenge) (bool, error)) (coreclient.Session, error) {
	if result := attempt.Start(request); result.Err != nil {
		return coreclient.Session{}, result.Err
	}
	// A creation response is acceptance only. Always verify readiness with GET.
	for {
		result := attempt.Poll()
		if result.Err != nil {
			return coreclient.Session{}, result.Err
		}
		session := result.Session
		if session.Done() {
			code := session.FailureCode
			if code == "" {
				code = session.FrameworkCode
			}
			if code == "" {
				code = "CONNECTION_FAILED"
			}
			return session, coreclient.Failure(code)
		}
		if session.Ready() {
			return session, nil
		}
		if session.State == "auth_pending" {
			return session, coreclient.Failure("AUTHENTICATION_FAILED")
		}
		if result.Challenge != nil {
			challenge := *result.Challenge
			if challenge.Type != "host_key" {
				return session, coreclient.Failure("PROTOCOL_ERROR")
			}
			accept, err := confirm(challenge)
			if err != nil {
				return session, err
			}
			if err := attempt.Answer("host-key", coreclient.ChallengeAnswer{ChallengeID: challenge.ID, Accept: accept, Remember: accept}); err != nil {
				return session, err
			}
			if !accept {
				return session, coreclient.Failure("HOST_KEY_VERIFICATION_FAILED")
			}
		} else if session.State != "connecting" && session.State != "host_key_pending" {
			return session, coreclient.Failure("SSH_STATE_UNKNOWN")
		}
		timer := time.NewTimer(300 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return session, ctx.Err()
		case <-timer.C:
		}
	}
}

func confirmHost(ctx context.Context, c coreclient.Challenge, tr i18n.Translator, input io.Reader, output io.Writer) (bool, error) {
	if !c.ExpiresAt.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, c.ExpiresAt)
		defer cancel()
	}
	key := i18n.Key("ssh.host_unknown")
	if c.Risk == "HIGH_RISK" {
		key = "ssh.host_changed"
	}
	if _, err := fmt.Fprintln(output, tr.Text(key, map[string]any{"Host": display(c.Host), "Alias": display(c.ServerAlias), "KeyType": display(c.KeyType), "Fingerprint": display(c.Fingerprint)})); err != nil {
		return false, coreclient.Failure("TERMINAL_FAILED")
	}
	for {
		if _, err := fmt.Fprint(output, tr.Message("ssh.host_confirm")); err != nil {
			return false, coreclient.Failure("TERMINAL_FAILED")
		}
		line, err := terminal.ReadLine(ctx, input)
		if errors.Is(err, context.DeadlineExceeded) {
			return false, coreclient.Failure("HOST_KEY_CHALLENGE_EXPIRED")
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return false, err
			}
			return false, coreclient.Failure("TERMINAL_FAILED")
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "yes":
			return true, nil
		case "no", "":
			return false, nil
		}
	}
}

func display(value string) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

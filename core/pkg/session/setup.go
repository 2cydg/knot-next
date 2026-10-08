package session

import (
	"context"
	"fmt"
	"sort"
	"time"

	"golang.org/x/crypto/ssh"

	"knot-core/internal/resourcepolicy"
)

// openSSHSession opens a channel on a pooled client under ctx. x/crypto/ssh has
// no cancellation for this call, so a cancelled attempt abandons the pending open
// and closes whatever channel it eventually returns; the shared client itself is
// never closed here, because another session may be using it.
const interactiveSetupTimeout = 15 * time.Second
const maxSSHEnvironmentEntries = 64

func openSSHSession(ctx context.Context, client *ssh.Client) (*ssh.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, interactiveSetupTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		session *ssh.Session
		err     error
	}
	done := make(chan result, 1)
	resourcepolicy.Go(ctx, func() {
		session, err := client.NewSession()
		done <- result{session: session, err: err}
	})
	select {
	case res := <-done:
		return res.session, res.err
	case <-ctx.Done():
		resourcepolicy.Go(ctx, func() {
			if res := <-done; res.session != nil {
				closeSSHSession(ctx, res.session)
			}
		})
		return nil, ctx.Err()
	}
}

// sessionSetup runs one blocking setup step on a session this attempt owns. A
// cancelled attempt closes that session, which unblocks the step; the step's own
// result is drained so the goroutine cannot leak.
func sessionSetup(ctx context.Context, session *ssh.Session, step func() error) error {
	ctx, cancel := context.WithTimeout(ctx, interactiveSetupTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	resourcepolicy.Go(ctx, func() { done <- step() })
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		closeSSHSession(ctx, session)
		resourcepolicy.Go(ctx, func() { <-done })
		return ctx.Err()
	}
}

// closeSSHSession owns asynchronous cleanup so a broken channel Close cannot
// pin cancellation. The lease is retained until tracked work actually settles.
func closeSSHSession(ctx context.Context, session *ssh.Session) {
	resourcepolicy.Go(ctx, func() { _ = session.Close() })
}

func setSSHSessionEnvironment(ctx context.Context, session *ssh.Session, env map[string]string, rejected func()) error {
	return setSSHSessionEnvironmentWithTimeout(ctx, session, env, rejected, interactiveSetupTimeout)
}

func setSSHSessionEnvironmentWithTimeout(ctx context.Context, session *ssh.Session, env map[string]string, rejected func(), timeout time.Duration) error {
	if len(env) > maxSSHEnvironmentEntries {
		return fmt.Errorf("%w: too many environment variables", ErrValidation)
	}
	// All requests share one phase deadline, including slow successful replies.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := env[key]
		if validSSHEnvName(key) && validSSHEnvValue(value) {
			var accepted bool
			if err := sessionSetup(ctx, session, func() error {
				var err error
				accepted, err = session.SendRequest("env", true, ssh.Marshal(struct{ Name, Value string }{key, value}))
				return err
			}); err != nil {
				return fmt.Errorf("set SSH environment: %w", err)
			}
			// A negative protocol reply is optional-env rejection. Transport
			// errors and cancellation above still terminate the owned channel.
			if !accepted && rejected != nil {
				rejected()
			}
		}
	}
	return nil
}

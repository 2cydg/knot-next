package sshpool

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"knot-core/internal/keyutil"
	"knot-core/internal/resourcepolicy"
	"knot-core/pkg/config"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/net/proxy"
)

var (
	ErrAuthFailed    = errors.New("authentication failed")
	ErrHostKeyReject = errors.New("host key verification failed")
)

type HostKeyError struct {
	Prompt HostKeyPrompt
}

func (e *HostKeyError) Error() string {
	if e == nil {
		return ErrHostKeyReject.Error()
	}
	if e.Prompt.Changed {
		return ErrHostKeyReject.Error() + ": host key changed"
	}
	return ErrHostKeyReject.Error() + ": unknown host"
}

func (e *HostKeyError) Unwrap() error {
	return ErrHostKeyReject
}

type AuthError struct {
	FailedMethod   string
	AllowedMethods []string
	Err            error
}

func (e *AuthError) Error() string {
	if e == nil {
		return ErrAuthFailed.Error()
	}
	if e.Err == nil {
		return ErrAuthFailed.Error()
	}
	return e.Err.Error()
}

func (e *AuthError) Unwrap() error {
	if e == nil || e.Err == nil {
		return ErrAuthFailed
	}
	return e.Err
}

const (
	HostKeyPolicyAsk          = ""
	HostKeyPolicyFail         = "fail"
	HostKeyPolicyAcceptNew    = "accept-new"
	HostKeyPolicyStrict       = "strict"
	HostKeyPolicyInsecureSkip = "insecure-skip"
)

type DialOptions struct {
	AgentSocket   string
	HostKeyPolicy string
	Timeout       time.Duration
	// prompt receives the effective caller and pool cancellation context.
	prompt func(context.Context, HostKeyPrompt) bool
}

type Pool struct {
	forwardAgents      map[*ssh.Client]string
	workers            resourcepolicy.Group
	callbacks          resourcepolicy.Callbacks
	mu                 sync.Mutex
	entries            map[string]*entry
	inflight           map[string]*inflightRoute
	idleTimeout        time.Duration
	closed             bool
	ConnectCallback    func(string, *ssh.Client)
	DisconnectCallback func(string)
	ctx                context.Context
	cancel             context.CancelFunc
}

// attemptGrace bounds how long CloseAll waits for interactive creations to
// unwind after cancelling them. It is a variable so tests can shorten it.
var attemptGrace = 2 * time.Second

type entry struct {
	client     *ssh.Client
	lastAccess time.Time
	refCount   int
	serverID   string
	alias      string
	remoteHost string
	chainKeys  []string
	// identity digests the material a client was authenticated with. A later
	// attempt whose identity differs must not reuse this connection, even though
	// the non-secret pool key is the same.
	identity []byte
}

// inflightRoute is one in-flight route creation shared by callers that do not
// need their own interactive prompt. It exists so a waiter can leave without
// disturbing the others, and so the last waiter to leave stops a creation that
// nobody is waiting for any more.
type inflightRoute struct {
	key     string // Actual publication key; it can differ after an identity rotation.
	done    chan struct{}
	client  *ssh.Client
	err     error
	created bool
	waiters int
	// identity is the digest the creation is authenticating with. A caller whose
	// own identity differs must not join it.
	identity []byte
	// cancel ends the creation itself. It is derived from the pool context, not
	// from any single caller, so a cancelled creator cannot fail the waiters.
	cancel context.CancelFunc
}

type EntryStat struct {
	Key       string   `json:"key"`
	ServerID  string   `json:"server_id"`
	Alias     string   `json:"alias"`
	Host      string   `json:"host"`
	IdleTime  string   `json:"idle_time"`
	RefCount  int      `json:"ref_count"`
	ChainKeys []string `json:"chain_keys,omitempty"`
}

var dialClient = dial

func NewPool() *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		entries:     map[string]*entry{},
		inflight:    map[string]*inflightRoute{},
		idleTimeout: 30 * time.Minute,
		ctx:         ctx,
		cancel:      cancel,
	}
	p.workers.Add()
	go func() { defer p.workers.Done(); p.cleanupLoop() }()
	return p
}

// GetClient returns a pooled client for server without a caller context. It is
// kept for callers that have no cancellation to propagate; new code should use
// AcquireClientContext so the returned route is already held and a cancelled
// attempt ends its dial and handshake.
func (p *Pool) GetClient(server config.ServerProfile, cfg config.RuntimeConfig, confirm func(HostKeyPrompt) bool, opts DialOptions) (*ssh.Client, []string, bool, error) {
	return p.GetClientContext(context.Background(), server, cfg, confirm, opts)
}

// GetClientContext returns an unowned pooled client for legacy callers. Core
// resources must use AcquireClientContext for atomic reference ownership.
//
// ctx bounds this caller's wait and its own creation attempt: a cancelled caller
// stops waiting, and stops a creation nobody else is waiting for. It never
// cancels a connection another session is already using. confirm, when non-nil,
// marks an interactive creation whose host key prompt belongs to this caller
// alone, so it is never merged with another session's in-flight attempt.
func (p *Pool) GetClientContext(ctx context.Context, server config.ServerProfile, cfg config.RuntimeConfig, confirm func(HostKeyPrompt) bool, opts DialOptions) (*ssh.Client, []string, bool, error) {
	lease, err := p.getClientContext(ctx, server, cfg, confirm, opts, false)
	if err != nil {
		return nil, nil, false, err
	}
	return lease.Client, lease.Keys, lease.Created, nil
}

// ClientLease owns one reference to every route entry. Release is idempotent and
// tied to the original entries, so Clear/recreation cannot release a new owner.
// Keep the lease until all channel and cleanup work has actually ended.
type ClientLease struct {
	Client   *ssh.Client
	Keys     []string
	Created  bool
	releases []func()
	entries  []*entry
	once     sync.Once
}

func (l *ClientLease) Release() {
	l.once.Do(func() {
		for i := len(l.releases) - 1; i >= 0; i-- {
			l.releases[i]()
		}
	})
}

// AcquireClientContext returns a client with its complete route already held.
// Unlike the legacy GetClientContext + IncRef sequence, identity replacement
// cannot intervene between returning the client and taking its references.
func (p *Pool) AcquireClientContext(ctx context.Context, server config.ServerProfile, cfg config.RuntimeConfig, confirm func(HostKeyPrompt) bool, opts DialOptions) (*ClientLease, error) {
	return p.getClientContext(ctx, server, cfg, confirm, opts, true)
}

func (p *Pool) AcquireClientContextWithPrompt(ctx context.Context, server config.ServerProfile, cfg config.RuntimeConfig, prompt func(context.Context, HostKeyPrompt) bool, opts DialOptions) (*ClientLease, error) {
	if prompt == nil {
		return p.AcquireClientContext(ctx, server, cfg, nil, opts)
	}
	opts.prompt = prompt
	return p.AcquireClientContext(ctx, server, cfg, nil, opts)
}

func (p *Pool) getClientContext(ctx context.Context, server config.ServerProfile, cfg config.RuntimeConfig, confirm func(HostKeyPrompt) bool, opts DialOptions, retain bool) (*ClientLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Settings.IdleTimeout != "" {
		if d, err := time.ParseDuration(cfg.Settings.IdleTimeout); err == nil {
			p.SetIdleTimeout(d)
		}
	}
	routes, err := routeChain(server, cfg)
	if err != nil {
		return nil, err
	}
	if opts.HostKeyPolicy != "" {
		for i := range routes {
			routes[i].key += "|host-key-policy=" + opts.HostKeyPolicy
		}
	}
	lease := &ClientLease{Keys: make([]string, 0, len(routes))}
	for _, route := range routes {
		for {
			if err := ctx.Err(); err != nil {
				lease.Release()
				return nil, err
			}
			client, key, created, err := p.getRouteClient(ctx, route, cfg, lease.Client, confirm, opts, lease.entries)
			if err != nil {
				lease.Release()
				return nil, err
			}
			if retain {
				ent, release, held := p.retainClient(key, client)
				if !held {
					// Idle replacement or Clear won before acquisition. Retry rather
					// than returning the old client or incrementing its replacement.
					continue
				}
				lease.entries = append(lease.entries, ent)
				lease.releases = append(lease.releases, release)
			}
			lease.Client = client
			lease.Keys = append(lease.Keys, key)
			lease.Created = lease.Created || created
			break
		}
	}
	p.setChainKeys(lease.Keys...)
	return lease, nil
}

// retainClient validates the exact client and increments its entry in the same
// critical section. The release captures that entry, never a reusable key.
func (p *Pool) retainClient(key string, client *ssh.Client) (*entry, func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ent := p.entries[key]
	if p.closed || ent == nil || ent.client != client {
		return nil, nil, false
	}
	ent.refCount++
	ent.lastAccess = time.Now()
	var once sync.Once
	return ent, func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			ent.refCount--
			ent.lastAccess = time.Now()
		})
	}, true
}

// retainPrefixLocked gives shared dial work its own references. A canceled
// waiter can release its lease while the pool-owned dial is still unwinding.
func (p *Pool) retainPrefixLocked(prefix []*entry) func() {
	if len(prefix) == 0 {
		return nil
	}
	entries := append([]*entry(nil), prefix...)
	for _, ent := range entries {
		ent.refCount++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			for _, ent := range entries {
				ent.refCount--
				ent.lastAccess = time.Now()
			}
		})
	}
}

// GetClientContextWithPrompt gives interactive callbacks the effective attempt
// context. A pending prompt must return when this context ends, including when
// the pool closes. Legacy GetClientContext callbacks have no context parameter.
func (p *Pool) GetClientContextWithPrompt(ctx context.Context, server config.ServerProfile, cfg config.RuntimeConfig, prompt func(context.Context, HostKeyPrompt) bool, opts DialOptions) (*ssh.Client, []string, bool, error) {
	if prompt == nil {
		return p.GetClientContext(ctx, server, cfg, nil, opts)
	}
	opts.prompt = prompt
	return p.GetClientContext(ctx, server, cfg, nil, opts)
}

func (p *Pool) SetIdleTimeout(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed && d > 0 {
		p.idleTimeout = d
	}
}

// IncRef is legacy key-based bookkeeping. It cannot atomically validate the
// client returned earlier by GetClient; core owners must use ClientLease instead.
func (p *Pool) IncRef(keys ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, key := range keys {
		if ent := p.entries[key]; ent != nil {
			ent.refCount++
			ent.lastAccess = time.Now()
		}
	}
}

func (p *Pool) DecRef(keys ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, key := range keys {
		if ent := p.entries[key]; ent != nil && ent.refCount > 0 {
			ent.refCount--
			ent.lastAccess = time.Now()
		}
	}
}

func (p *Pool) Touch(keys ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, key := range keys {
		if ent := p.entries[key]; ent != nil {
			ent.lastAccess = now
		}
	}
}

func (p *Pool) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

func (p *Pool) IsClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *Pool) Stats() []EntryStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statsLocked(time.Now())
}

func (p *Pool) Snapshot() (int, []EntryStat) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries), p.statsLocked(time.Now())
}

func (p *Pool) HasKey(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.entries[key]
	return ok
}

func (p *Pool) Clear() int {
	p.mu.Lock()
	entries := p.entries
	count := len(entries)
	p.entries = map[string]*entry{}
	p.mu.Unlock()
	p.closeEntries(entries, true)
	return count
}

// stop detaches entries once and schedules transport close outside the pool lock.
func (p *Pool) stop() int {
	p.cancel()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0
	}
	p.closed = true
	// Observers are advisory and may reenter Shutdown. Drop queued notifications
	// without waiting for the current observer; transport workers own cleanup.
	p.callbacks.Close()
	entries := p.entries
	p.entries = map[string]*entry{}
	p.workers.Add()
	p.mu.Unlock()
	// Shutdown suppresses advisory disconnects; Clear and idle cleanup still
	// request notifications while their dispatcher is open.
	go func() { defer p.workers.Done(); p.closeEntries(entries, false) }()
	return len(entries)
}
func (p *Pool) CloseAll() int {
	count := p.stop()
	p.waitInteractive(attemptGrace)
	return count
}

// Shutdown reports an incomplete release instead of silently treating a grace
// timeout as success. All dial, cleanup and keepalive workers are owned here.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.stop()
	return p.workers.Wait(ctx)
}
func (p *Pool) waitInteractive(grace time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	_ = p.workers.Wait(ctx)
}

func (p *Pool) statsLocked(now time.Time) []EntryStat {
	stats := make([]EntryStat, 0, len(p.entries))
	for key, ent := range p.entries {
		stats = append(stats, EntryStat{
			Key:       key,
			ServerID:  ent.serverID,
			Alias:     ent.alias,
			Host:      ent.remoteHost,
			IdleTime:  now.Sub(ent.lastAccess).Round(time.Second).String(),
			RefCount:  ent.refCount,
			ChainKeys: cloneStrings(ent.chainKeys),
		})
	}
	return stats
}

func (p *Pool) setChainKeys(keys ...string) {
	if len(keys) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	chain := cloneStrings(keys)
	for _, key := range keys {
		if ent := p.entries[key]; ent != nil {
			ent.chainKeys = cloneStrings(chain)
		}
	}
}

func (p *Pool) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.cleanupIdle()
		}
	}
}

func (p *Pool) keepAliveLoop(key string, client *ssh.Client, cfg config.RuntimeConfig) {
	interval := 20 * time.Second
	if cfg.Settings.KeepaliveInterval != "" {
		if d, err := time.ParseDuration(cfg.Settings.KeepaliveInterval); err == nil {
			interval = d
		}
	}

	done := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(done)
	}()
	defer func() { <-done }()
	if interval <= 0 {
		select {
		case <-done:
			p.dropEntryIfMatch(key, client, true)
		case <-p.ctx.Done():
		}
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := sendKeepAlive(client); err != nil {
				slog.Debug("ssh keepalive request failed", "key", key, "error", err)
			}
		case <-done:
			p.dropEntryIfMatch(key, client, true)
			return
		case <-p.ctx.Done():
			return
		}
	}
}

func sendKeepAlive(client *ssh.Client) error {
	_, _, err := client.SendRequest("keepalive@openssh.com", false, nil)
	return err
}

func (p *Pool) cleanupIdle() {
	now := time.Now()
	stale := map[string]*entry{}
	p.mu.Lock()
	for key, ent := range p.entries {
		if ent.refCount == 0 && now.Sub(ent.lastAccess) > p.idleTimeout {
			delete(p.entries, key)
			stale[key] = ent
		}
	}
	p.mu.Unlock()
	p.closeEntries(stale, true)
}

func (p *Pool) dropEntryIfMatch(key string, client *ssh.Client, notify bool) bool {
	p.mu.Lock()
	ent, ok := p.entries[key]
	if !ok || ent.client != client {
		p.mu.Unlock()
		return false
	}
	delete(p.entries, key)
	p.mu.Unlock()

	_ = client.Close()
	if notify {
		p.notifyDisconnect(key)
	}
	return true
}

func (p *Pool) closeEntries(entries map[string]*entry, notify bool) {
	for key, ent := range entries {
		_ = ent.client.Close()
		if notify {
			p.notifyDisconnect(key)
		}
	}
}

func (p *Pool) notifyConnect(key string, client *ssh.Client) {
	if p.ConnectCallback == nil {
		return
	}
	callback := p.ConnectCallback
	p.callbacks.Send(func() { callback(key, client) })
}

func (p *Pool) notifyDisconnect(key string) {
	if p.DisconnectCallback == nil {
		return
	}
	callback := p.DisconnectCallback
	p.callbacks.Send(func() { callback(key) })
}

func (p *Pool) IsAlive(key string, client *ssh.Client) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	ent, ok := p.entries[key]
	return ok && ent.client == client
}

// getRouteClient returns a usable client for one step of the route chain
// together with the pool key it is registered under. The key can differ from
// route.key when the stored identity no longer matches the credentials of this
// attempt, so callers must use the returned key for any later reference
// bookkeeping.
func (p *Pool) getRouteClient(ctx context.Context, route routeStep, cfg config.RuntimeConfig, jump *ssh.Client, confirm func(HostKeyPrompt) bool, opts DialOptions, prefix []*entry) (*ssh.Client, string, bool, error) {
	identityProfile := route.server
	if route.via != nil {
		identityProfile.JumpHostIDs = route.via
	}
	identity := clientIdentity(identityProfile, cfg, opts)

	// An interactive attempt owns its own creation: its host key prompt belongs
	// to this caller, so it must never be merged with another session's attempt.
	if confirm != nil || opts.prompt != nil {
		key, _, err := p.resolveKey(route.key, identity)
		if err != nil {
			return nil, route.key, false, err
		}
		if client := p.reusableClient(key, identity); client != nil {
			return client, key, false, nil
		}
		// The attempt ends with the pool as well as with its caller. CloseAll
		// cancels the pool context, and without this the dial below would keep
		// running: an interactive attempt is not registered as an inflight route,
		// so nothing else would reach it.
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		defer cancelAttempt()
		stopPoolWatch := context.AfterFunc(p.ctx, cancelAttempt)
		defer stopPoolWatch()

		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, key, false, errors.New("ssh pool is closed")
		}
		p.workers.Add()
		p.mu.Unlock()
		defer p.workers.Done()

		effectiveConfirm := func(prompt HostKeyPrompt) bool {
			if opts.prompt != nil {
				return opts.prompt(attemptCtx, prompt)
			}
			// A legacy callback cannot be interrupted; stop the dial's wait and discard
			// its answer. New core callers use the context-aware API above.
			answered := make(chan bool, 1)
			go func() { answered <- confirm(prompt) }()
			select {
			case accept := <-answered:
				return accept
			case <-attemptCtx.Done():
				return false
			}
		}
		client, err := dialClient(attemptCtx, route.server, cfg, jump, effectiveConfirm, opts)
		if err != nil {
			return nil, key, false, err
		}
		// The pool may have closed while this attempt was dialling. Publishing
		// then would leave a live client in a pool that is already torn down, so
		// the connection is closed instead.
		if err := attemptCtx.Err(); err != nil {
			_ = client.Close()
			return nil, key, false, err
		}
		publishedKey, use, created, err := p.publish(key, route, identity, client, cfg)
		if err != nil {
			_ = client.Close()
			return nil, key, false, err
		}
		return use, publishedKey, created, nil
	}

	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, route.key, false, errors.New("ssh pool is closed")
		}
		key, client, inflight := p.resolveLocked(route.key, identity)
		if client != nil {
			ent := p.entries[key]
			ent.lastAccess = time.Now()
			p.mu.Unlock()
			return client, key, false, nil
		}
		if inflight == nil {
			createCtx, cancel := context.WithCancel(p.ctx)
			inflight = &inflightRoute{
				done:     make(chan struct{}),
				waiters:  1,
				identity: identity,
				cancel:   cancel,
			}
			p.inflight[key] = inflight
			p.workers.Add()
			releasePrefix := p.retainPrefixLocked(prefix)
			p.mu.Unlock()
			go p.runCreation(createCtx, key, route, identity, cfg, jump, opts, inflight, releasePrefix)
		} else {
			inflight.waiters++
			p.mu.Unlock()
		}

		client, created, err := p.awaitInflight(ctx, key, inflight)
		if err != nil {
			return nil, key, false, err
		}
		return client, inflight.key, created, nil
	}
}

// runCreation performs one shared route creation and publishes its result into
// the in-flight entry. It runs under a context derived from the pool, never from
// a single caller, so a caller that gives up cannot fail the others still
// waiting on this connection.
func (p *Pool) runCreation(ctx context.Context, key string, route routeStep, identity []byte, cfg config.RuntimeConfig, jump *ssh.Client, opts DialOptions, inflight *inflightRoute, releasePrefix func()) {
	defer p.workers.Done()
	if releasePrefix != nil {
		defer releasePrefix()
	}
	client, err := dialClient(ctx, route.server, cfg, jump, nil, opts)
	if err == nil && ctx.Err() != nil {
		// The creation outlived its purpose; do not publish a client nobody is
		// waiting for any more.
		_ = client.Close()
		err = ctx.Err()
	}
	created := false
	publishedKey := key
	if err == nil {
		var published *ssh.Client
		if publishedKey, published, created, err = p.publish(key, route, identity, client, cfg); err != nil {
			_ = client.Close()
		} else {
			client = published
		}
	}

	// The actual dial/setup has ended. Return the worker's prefix references
	// before publishing completion to callers, including canceled ones. The
	// deferred, idempotent release also covers future early returns.
	if releasePrefix != nil {
		releasePrefix()
	}
	p.mu.Lock()
	inflight.key = publishedKey
	inflight.client = client
	inflight.err = err
	inflight.created = created
	// The in-flight entry is always registered under the key the creation
	// started with, whatever key the client itself landed on.
	if p.inflight[key] == inflight {
		delete(p.inflight, key)
	}
	cancel := inflight.cancel
	close(inflight.done)
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// awaitInflight waits for a shared creation to finish. A caller that gives up
// stops waiting at once, and its departure cancels a creation nobody is waiting
// for any more, without touching the connections other sessions already hold.
func (p *Pool) awaitInflight(ctx context.Context, key string, inflight *inflightRoute) (*ssh.Client, bool, error) {
	select {
	case <-inflight.done:
		p.leaveInflight(key, inflight)
		return inflight.client, inflight.created, inflight.err
	case <-ctx.Done():
		p.leaveInflight(key, inflight)
		return nil, false, ctx.Err()
	case <-p.ctx.Done():
		p.leaveInflight(key, inflight)
		return nil, false, errors.New("ssh pool is closed")
	}
}

// leaveInflight releases one waiter. The last waiter to leave ends the creation,
// because the connection it is producing has no consumer left.
func (p *Pool) leaveInflight(key string, inflight *inflightRoute) {
	p.mu.Lock()
	if p.inflight[key] != inflight {
		p.mu.Unlock()
		return
	}
	inflight.waiters--
	if inflight.waiters > 0 {
		p.mu.Unlock()
		return
	}
	inflight.waiters = 0
	cancel := inflight.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// resolveKey returns the pool key an attempt with this identity may be published
// under, without consulting the in-flight registry.
func (p *Pool) resolveKey(base string, identity []byte) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return base, false, errors.New("ssh pool is closed")
	}
	key, _, _ := p.resolveLocked(base, identity)
	return key, true, nil
}

// reusableClient returns the pooled client at key when it was authenticated with
// the same material as this attempt.
func (p *Pool) reusableClient(key string, identity []byte) *ssh.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	ent := p.entries[key]
	if ent == nil || !bytes.Equal(ent.identity, identity) {
		return nil
	}
	ent.lastAccess = time.Now()
	return ent.client
}

// resolveLocked picks the key this attempt may use, the client it may reuse, and
// the in-flight creation it may join. The base key is preferred; when it holds
// different material and a session is still using it, a revision key carries the
// new material so the live session keeps the connection it was authenticated
// with. The caller must hold p.mu.
func (p *Pool) resolveLocked(base string, identity []byte) (string, *ssh.Client, *inflightRoute) {
	for rev := 0; ; rev++ {
		key := base
		if rev > 0 {
			key = fmt.Sprintf("%s|rev=%d", base, rev)
		}
		ent := p.entries[key]
		if ent != nil {
			if bytes.Equal(ent.identity, identity) {
				return key, ent.client, nil
			}
			if ent.refCount > 0 {
				// In use with other material: keep it and move on.
				continue
			}
		}
		inflight := p.inflight[key]
		if inflight != nil && !bytes.Equal(inflight.identity, identity) {
			// Being created with other material: it owns this key.
			continue
		}
		return key, nil, inflight
	}
}

// publish stores a freshly created client and reports the key it landed under,
// the client that key now holds, and whether that client is the new one. When an
// equivalent connection appeared while this one was being built, the existing
// client wins and the new one is closed here.
func (p *Pool) publish(base string, route routeStep, identity []byte, client *ssh.Client, cfg config.RuntimeConfig) (string, *ssh.Client, bool, error) {
	now := time.Now()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return base, nil, false, errors.New("ssh pool is closed")
	}
	key := base
	if ent := p.entries[key]; ent != nil && ent.refCount > 0 && !bytes.Equal(ent.identity, identity) {
		// Defensive: a live session took the slot while this client was being
		// built. Keep its connection and give the new material its own key.
		for rev := 1; ; rev++ {
			candidate := fmt.Sprintf("%s|rev=%d", base, rev)
			if cur := p.entries[candidate]; cur == nil || bytes.Equal(cur.identity, identity) {
				key = candidate
				break
			}
		}
	}
	if ent := p.entries[key]; ent != nil && bytes.Equal(ent.identity, identity) {
		ent.lastAccess = now
		existing := ent.client
		p.mu.Unlock()
		_ = client.Close()
		return key, existing, false, nil
	}
	var stale *ssh.Client
	if ent := p.entries[key]; ent != nil {
		stale = ent.client
	}
	p.entries[key] = &entry{
		client:     client,
		lastAccess: now,
		serverID:   route.server.ID,
		alias:      route.server.Alias,
		remoteHost: route.server.Host,
		chainKeys:  []string{key},
		identity:   identity,
	}
	p.workers.Add()
	p.mu.Unlock()

	if stale != nil && stale != client {
		_ = stale.Close()
		// The replaced entry was idle. Its key now belongs to the new client;
		// a delayed disconnect callback would close that client's new owners.
	}
	go func() { defer p.workers.Done(); p.keepAliveLoop(key, client, cfg) }()
	p.notifyConnect(key, client)
	return key, client, true, nil
}

// clientIdentity digests the material a connection is authenticated with: the
// credentials, the proxy credentials and the options that decide whether a
// pooled connection may serve a later attempt. Only the digest is kept, so no
// secret reaches a pool key or the stats exposed over the API.
//
// The digest covers the whole route, not just the target: a connection through a
// jump host was authenticated by that jump host too, so rotating the jump's
// password or key must invalidate the target's cached connection instead of
// letting a later attempt reuse one that still runs over the old route.
func clientIdentity(server config.ServerProfile, cfg config.RuntimeConfig, opts DialOptions) []byte {
	h := sha256.New()
	identityFields(h, server, cfg, opts)
	for _, id := range server.JumpHostIDs {
		jump, ok := cfg.Servers[id]
		if !ok {
			// A route that cannot be resolved must not digest the same as one
			// whose jump host is present but identical.
			fmt.Fprintf(h, "jump-missing=%s\n", id)
			continue
		}
		fmt.Fprintf(h, "jump=%s\n", id)
		identityFields(h, jump, cfg, opts)
	}
	return h.Sum(nil)
}

// identityFields folds one server profile's authentication material into the
// identity digest.
func identityFields(h io.Writer, server config.ServerProfile, cfg config.RuntimeConfig, opts DialOptions) {
	field := func(label, value string) {
		fmt.Fprintf(h, "%s=%d:%s\n", label, len(value), value)
	}
	field("id", server.ID)
	field("host", server.Host)
	field("port", strconv.Itoa(server.Port))
	field("user", server.User)
	field("auth", server.AuthMethod)
	field("password", server.Password)
	field("key", server.KeyID)
	if key, ok := cfg.Keys[server.KeyID]; ok {
		field("key-private", key.PrivateKey)
		field("key-passphrase", key.Passphrase)
		field("key-source", key.SourcePath)
		field("key-type", key.Type)
		// The path alone is not the key: replacing the file at that path changes
		// what the next authentication uses, so the bytes decide the identity.
		field("key-source-digest", sourceKeyDigest(key.SourcePath))
	}
	field("agent-socket", opts.AgentSocket)
	field("host-key-policy", opts.HostKeyPolicy)
	field("known-hosts", server.KnownHostsPath)
	if proxyCfg, ok := cfg.Proxies[server.ProxyID]; ok {
		field("proxy-type", proxyCfg.Type)
		field("proxy-host", proxyCfg.Host)
		field("proxy-port", strconv.Itoa(proxyCfg.Port))
		field("proxy-user", proxyCfg.Username)
		field("proxy-password", proxyCfg.Password)
	}
}

// sourceKeyDigest digests an external key file's contents. A key that cannot be
// read is reported as unreadable rather than as an empty file, so a missing key
// and a present-but-unreadable one do not share an identity with an in-memory
// key.
func sourceKeyDigest(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "unreadable"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func dial(ctx context.Context, server config.ServerProfile, cfg config.RuntimeConfig, jump *ssh.Client, confirm func(HostKeyPrompt) bool, opts DialOptions) (*ssh.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	auth, closer, err := authMethodsContext(ctx, server, cfg, opts)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	addr := net.JoinHostPort(server.Host, strconv.Itoa(server.Port))

	// The transport phase carries its own deadline: the proxy negotiation and the
	// jump host dial must not outlive the attempt.
	conn, err := dialTransport(ctx, addr, server, cfg, jump, timeout)
	if err != nil {
		return nil, err
	}
	hostKeyCallback, err := hostKeyCallback(server, confirm, opts.HostKeyPolicy, &handshakeDeadline{conn: conn, timeout: timeout})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	clientConfig := &ssh.ClientConfig{
		User:            server.User,
		Auth:            auth,
		HostKeyCallback: hostKeyCallback,
		Timeout:         timeout,
	}

	// ssh.ClientConfig.Timeout only bounds the initial Dial, which this pool
	// performs itself; the handshake run by NewClientConn is bounded by the socket
	// deadline instead. A caller that gives up closes the socket, because the
	// handshake has no cancellation of its own.
	_ = conn.SetDeadline(time.Now().Add(timeout))
	stopWatch := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopWatch()

	ncc, chans, reqs, err := ssh.NewClientConn(conn, addr, clientConfig)
	if err != nil {
		_ = conn.Close()
		if strings.Contains(err.Error(), "ssh: unable to authenticate") || (server.AuthMethod == config.AuthMethodAgent && strings.Contains(err.Error(), "failed to sign")) {
			return nil, &AuthError{
				FailedMethod:   server.AuthMethod,
				AllowedMethods: nil,
				Err:            fmt.Errorf("%w: %v", ErrAuthFailed, err),
			}
		}
		return nil, err
	}
	// The handshake is done: clear the phase deadline so the pooled connection is
	// not killed mid-session, and stop watching so a later cancellation of the
	// same context cannot close a connection that is now shared.
	stopWatch()
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(ncc, chans, reqs), nil
}

func authMethods(server config.ServerProfile, cfg config.RuntimeConfig, opts DialOptions) ([]ssh.AuthMethod, io.Closer, error) {
	return authMethodsContext(context.Background(), server, cfg, opts)
}
func authMethodsContext(ctx context.Context, server config.ServerProfile, cfg config.RuntimeConfig, opts DialOptions) ([]ssh.AuthMethod, io.Closer, error) {
	switch server.AuthMethod {
	case config.AuthMethodPassword:
		if server.Password == "" {
			return nil, nil, &AuthError{
				FailedMethod:   config.AuthMethodPassword,
				AllowedMethods: []string{config.AuthMethodPassword, config.AuthMethodKey, config.AuthMethodAgent},
				Err:            fmt.Errorf("%w: password is not configured", ErrAuthFailed),
			}
		}
		return []ssh.AuthMethod{ssh.Password(server.Password)}, nil, nil
	case config.AuthMethodKey:
		key, ok := cfg.Keys[server.KeyID]
		if !ok {
			return nil, nil, &AuthError{
				FailedMethod:   config.AuthMethodKey,
				AllowedMethods: []string{config.AuthMethodPassword, config.AuthMethodKey, config.AuthMethodAgent},
				Err:            fmt.Errorf("%w: key %s was not found", ErrAuthFailed, server.KeyID),
			}
		}
		privateKey := key.PrivateKey
		if privateKey == "" && key.SourcePath != "" {
			raw, err := os.ReadFile(key.SourcePath)
			if err != nil {
				return nil, nil, err
			}
			privateKey = string(raw)
		}
		if privateKey == "" {
			return nil, nil, &AuthError{
				FailedMethod:   config.AuthMethodKey,
				AllowedMethods: []string{config.AuthMethodPassword, config.AuthMethodKey, config.AuthMethodAgent},
				Err:            fmt.Errorf("%w: private key is not configured", ErrAuthFailed),
			}
		}
		signer, err := keyutil.Signer([]byte(privateKey), key.Passphrase)
		if err != nil {
			return nil, nil, &AuthError{FailedMethod: config.AuthMethodKey, AllowedMethods: []string{config.AuthMethodKey, config.AuthMethodPassword, config.AuthMethodAgent}, Err: fmt.Errorf("%w: %w", ErrAuthFailed, err)}
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil, nil
	case config.AuthMethodAgent, "":
		socket := opts.AgentSocket
		if socket == "" {
			socket = defaultAgentSocket()
		}
		if socket == "" {
			return nil, nil, &AuthError{
				FailedMethod:   config.AuthMethodAgent,
				AllowedMethods: []string{config.AuthMethodPassword, config.AuthMethodKey, config.AuthMethodAgent},
				Err:            fmt.Errorf("%w: SSH_AUTH_SOCK is not set", ErrAuthFailed),
			}
		}
		conn, err := openAgent(ctx, socket, opts.Timeout)
		if err != nil {
			return nil, nil, &AuthError{
				FailedMethod:   config.AuthMethodAgent,
				AllowedMethods: []string{config.AuthMethodPassword, config.AuthMethodKey, config.AuthMethodAgent},
				Err:            fmt.Errorf("%w: ssh agent unavailable", ErrAuthFailed),
			}
		}
		signers, err := agent.NewClient(conn).Signers()
		if err != nil || len(signers) == 0 {
			_ = conn.Close()
			return nil, nil, &AuthError{
				FailedMethod:   config.AuthMethodAgent,
				AllowedMethods: []string{config.AuthMethodPassword, config.AuthMethodKey, config.AuthMethodAgent},
				Err:            fmt.Errorf("%w: ssh agent has no usable signers", ErrAuthFailed),
			}
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, conn, nil
	default:
		return nil, nil, &AuthError{
			FailedMethod:   server.AuthMethod,
			AllowedMethods: []string{config.AuthMethodPassword, config.AuthMethodKey, config.AuthMethodAgent},
			Err:            fmt.Errorf("%w: unsupported auth method %q", ErrAuthFailed, server.AuthMethod),
		}
	}
}

// dialTransport opens the socket for one route step. Every phase - the jump host
// dial, the proxy negotiation and the direct dial - is bounded by ctx and by the
// phase timeout, so a cancelled attempt does not leave a socket behind.
func dialTransport(ctx context.Context, addr string, server config.ServerProfile, cfg config.RuntimeConfig, jump *ssh.Client, timeout time.Duration) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if jump != nil {
		return dialViaJump(ctx, jump, addr)
	}
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	if server.ProxyID == "" {
		return dialer.DialContext(ctx, "tcp", addr)
	}
	proxyCfg, ok := cfg.Proxies[server.ProxyID]
	if !ok {
		return nil, fmt.Errorf("proxy %s was not found", server.ProxyID)
	}
	proxyAddr := net.JoinHostPort(proxyCfg.Host, strconv.Itoa(proxyCfg.Port))
	switch proxyCfg.Type {
	case config.ProxyTypeSOCKS5:
		return dialSOCKS5(ctx, proxyAddr, addr, proxyCfg, dialer, timeout)
	case config.ProxyTypeHTTP:
		return dialHTTPProxy(ctx, proxyAddr, addr, proxyCfg.Username, proxyCfg.Password, dialer, timeout)
	default:
		return nil, fmt.Errorf("unsupported proxy type %q", proxyCfg.Type)
	}
}

// dialViaJump dials through a pooled jump client. x/crypto/ssh offers no
// cancellation for this call, so a cancelled attempt abandons the pending dial
// and closes whatever it eventually returns.
func dialViaJump(ctx context.Context, jump *ssh.Client, addr string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := jump.Dial("tcp", addr)
		done <- result{conn: conn, err: err}
	}()
	select {
	case res := <-done:
		return res.conn, res.err
	case <-ctx.Done():
		go func() {
			if res := <-done; res.conn != nil {
				_ = res.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// fixedDialer hands out one already-connected socket, so the SOCKS5 negotiation
// runs on the socket whose deadline bounds it rather than on a fresh connection.
type fixedDialer struct {
	conn net.Conn
	used bool
}

func (d *fixedDialer) Dial(network, address string) (net.Conn, error) {
	if d.used {
		return nil, errors.New("proxy dialer is already used")
	}
	d.used = true
	return d.conn, nil
}

func dialSOCKS5(ctx context.Context, proxyAddr string, targetAddr string, proxyCfg config.ProxyProfile, dialer *net.Dialer, timeout time.Duration) (net.Conn, error) {
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	var auth *proxy.Auth
	if proxyCfg.Username != "" {
		auth = &proxy.Auth{User: proxyCfg.Username, Password: proxyCfg.Password}
	}
	d, err := proxy.SOCKS5("tcp", proxyAddr, auth, &fixedDialer{conn: conn})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	target, err := d.Dial("tcp", targetAddr)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = target.SetDeadline(time.Time{})
	return target, nil
}

func dialHTTPProxy(ctx context.Context, proxyAddr string, targetAddr string, user string, pass string, dialer *net.Dialer, timeout time.Duration) (net.Conn, error) {
	if strings.ContainsAny(targetAddr+user+pass, "\r\n") {
		return nil, errors.New("invalid proxy CONNECT parameters")
	}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	// The CONNECT exchange is a phase of its own: bound it, then hand the socket
	// over to the handshake, which arms its own deadline.
	_ = conn.SetDeadline(time.Now().Add(timeout))
	authHeader := ""
	if user != "" {
		authHeader = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)) + "\r\n"
	}
	req := "CONNECT " + targetAddr + " HTTP/1.1\r\nHost: " + targetAddr + "\r\n" + authHeader + "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	parts := strings.SplitN(strings.TrimSpace(status), " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") || parts[1] != "200" {
		_ = conn.Close()
		return nil, fmt.Errorf("HTTP proxy connection failed: %s", strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	if reader.Buffered() > 0 {
		return &bufferedConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

type HostKeyPrompt struct {
	Host        string `json:"host"`
	KeyType     string `json:"key_type"`
	Fingerprint string `json:"fingerprint"`
	Changed     bool   `json:"changed"`
	Message     string `json:"message"`
}

// handshakeDeadline parks and re-arms the connection phase deadline around a host
// key prompt. Without it the phase timeout would fire while a human is deciding,
// killing a connection the client is still willing to accept.
type handshakeDeadline struct {
	conn    net.Conn
	timeout time.Duration
}

func (d *handshakeDeadline) park() {
	if d == nil || d.conn == nil {
		return
	}
	_ = d.conn.SetDeadline(time.Time{})
}

func (d *handshakeDeadline) arm() {
	if d == nil || d.conn == nil || d.timeout <= 0 {
		return
	}
	_ = d.conn.SetDeadline(time.Now().Add(d.timeout))
}

// ask runs confirm with the phase deadline parked, so the prompt may take as long
// as the client needs, and re-arms it once the answer is in.
func (d *handshakeDeadline) ask(confirm func(HostKeyPrompt) bool, prompt HostKeyPrompt) bool {
	d.park()
	defer d.arm()
	return confirm(prompt)
}

func hostKeyCallback(server config.ServerProfile, confirm func(HostKeyPrompt) bool, policy string, deadline *handshakeDeadline) (ssh.HostKeyCallback, error) {
	policy, err := normalizeHostKeyPolicy(policy)
	if err != nil {
		return nil, err
	}
	if policy == HostKeyPolicyInsecureSkip {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	khPath, err := knownHostsPath(server)
	if err != nil {
		return nil, err
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if err := os.MkdirAll(filepath.Dir(khPath), 0o700); err != nil {
			return err
		}
		callback, err := knownhosts.New(khPath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := ensureFile(khPath, 0o600); err != nil {
				return err
			}
			callback, err = knownhosts.New(khPath)
			if err != nil {
				return err
			}
		}
		if err := callback(hostname, remote, key); err == nil {
			return nil
		} else {
			var keyErr *knownhosts.KeyError
			if !errors.As(err, &keyErr) {
				return fmt.Errorf("%w: %v", ErrHostKeyReject, err)
			}
			changed := len(keyErr.Want) > 0
			switch {
			case changed:
				prompt := newHostKeyPrompt(hostname, key, true)
				if policy == HostKeyPolicyAsk && confirm != nil && deadline.ask(confirm, prompt) {
					return replaceKnownHost(khPath, hostname, key, keyErr.Want)
				}
				return &HostKeyError{Prompt: prompt}
			case policy == HostKeyPolicyAcceptNew:
				return appendKnownHost(khPath, hostname, key)
			case policy == HostKeyPolicyAsk && confirm != nil:
				prompt := newHostKeyPrompt(hostname, key, false)
				if deadline.ask(confirm, prompt) {
					return appendKnownHost(khPath, hostname, key)
				}
				return &HostKeyError{Prompt: prompt}
			}
			return fmt.Errorf("%w: unknown host", ErrHostKeyReject)
		}
	}, nil
}

func normalizeHostKeyPolicy(policy string) (string, error) {
	switch policy {
	case HostKeyPolicyAsk, HostKeyPolicyFail, HostKeyPolicyStrict, HostKeyPolicyAcceptNew, HostKeyPolicyInsecureSkip:
		return policy, nil
	default:
		return "", fmt.Errorf("invalid host key policy %q", policy)
	}
}

func knownHostsPath(server config.ServerProfile) (string, error) {
	if server.KnownHostsPath != "" {
		return server.KnownHostsPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "known_hosts"), nil
}

func newHostKeyPrompt(host string, key ssh.PublicKey, changed bool) HostKeyPrompt {
	msg := "unknown host key"
	if changed {
		msg = "remote host identification has changed"
	}
	return HostKeyPrompt{
		Host:        host,
		KeyType:     key.Type(),
		Fingerprint: ssh.FingerprintSHA256(key),
		Changed:     changed,
		Message:     msg,
	}
}

func appendKnownHost(path string, hostname string, key ssh.PublicKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	line := knownhosts.Line([]string{hostname}, key)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.WriteString(f, line+"\n")
	return err
}

func replaceKnownHost(path string, hostname string, key ssh.PublicKey, want []knownhosts.KnownKey) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	removeLines := map[int]struct{}{}
	for _, known := range want {
		if known.Filename == path && known.Line > 0 {
			removeLines[known.Line] = struct{}{}
		}
	}
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := scanner.Text()
		if _, ok := removeLines[lineNo]; ok || knownHostLineMatches(line, hostname) {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	lines = append(lines, knownhosts.Line([]string{hostname}, key))
	tmp := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func knownHostLineMatches(line string, hostname string) bool {
	fields := strings.Fields(line)
	if len(fields) < 3 || strings.HasPrefix(fields[0], "#") || strings.HasPrefix(fields[0], "@") || strings.HasPrefix(fields[0], "|") {
		return false
	}
	target := knownhosts.Normalize(hostname)
	for _, host := range strings.Split(fields[0], ",") {
		host = strings.TrimPrefix(host, "!")
		if knownhosts.Normalize(host) == target {
			return true
		}
	}
	return false
}

func ensureFile(path string, perm os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	return file.Close()
}

type routeStep struct {
	server config.ServerProfile
	key    string
	via    []string
}

func routeChain(server config.ServerProfile, cfg config.RuntimeConfig) ([]routeStep, error) {
	if len(server.JumpHostIDs) == 0 {
		return []routeStep{{server: server, key: connKey(server)}}, nil
	}
	out := make([]routeStep, 0, len(server.JumpHostIDs)+1)
	via := make([]string, 0, len(server.JumpHostIDs))
	for _, id := range server.JumpHostIDs {
		jump, ok := cfg.Servers[id]
		if !ok {
			return nil, fmt.Errorf("jump host %s was not found", id)
		}
		key := connKey(jump)
		if len(via) > 0 {
			key += "|via=" + strings.Join(via, "->")
		}
		out = append(out, routeStep{server: jump, key: key, via: append([]string{}, via...)})
		via = append(via, id)
	}
	out = append(out, routeStep{server: server, key: connKey(server) + "|via=" + strings.Join(via, "->")})
	return out, nil
}

func connKey(server config.ServerProfile) string {
	sum := sha256.Sum256([]byte(server.KnownHostsPath + "|" + server.ProxyID + "|" + strings.Join(server.JumpHostIDs, ",")))
	return fmt.Sprintf("%s:%s@%s:%d:%x", server.ID, server.User, server.Host, server.Port, sum[:4])
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrAuthFailed) || strings.Contains(err.Error(), "ssh: unable to authenticate")
}

func AuthFailureDetails(err error) (string, []string, bool) {
	var authErr *AuthError
	if errors.As(err, &authErr) {
		return authErr.FailedMethod, cloneStrings(authErr.AllowedMethods), true
	}
	return "", nil, false
}

func HostKeyPromptFromError(err error) (HostKeyPrompt, bool) {
	var hostErr *HostKeyError
	if errors.As(err, &hostErr) && hostErr != nil {
		return hostErr.Prompt, true
	}
	return HostKeyPrompt{}, false
}

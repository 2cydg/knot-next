package sshpool

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
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

	"knot-core/pkg/config"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/net/proxy"
	"golang.org/x/sync/singleflight"
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
}

type Pool struct {
	mu                 sync.Mutex
	entries            map[string]*entry
	sf                 singleflight.Group
	idleTimeout        time.Duration
	closed             bool
	ConnectCallback    func(string, *ssh.Client)
	DisconnectCallback func(string)
	ctx                context.Context
	cancel             context.CancelFunc
}

type entry struct {
	client     *ssh.Client
	lastAccess time.Time
	refCount   int
	serverID   string
	alias      string
	remoteHost string
	chainKeys  []string
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

type getClientResult struct {
	client *ssh.Client
	isNew  bool
}

var dialClient = dial

func NewPool() *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		entries:     map[string]*entry{},
		idleTimeout: 30 * time.Minute,
		ctx:         ctx,
		cancel:      cancel,
	}
	go p.cleanupLoop()
	return p
}

func (p *Pool) GetClient(server config.ServerProfile, cfg config.RuntimeConfig, confirm func(HostKeyPrompt) bool, opts DialOptions) (*ssh.Client, []string, bool, error) {
	if cfg.Settings.IdleTimeout != "" {
		if d, err := time.ParseDuration(cfg.Settings.IdleTimeout); err == nil {
			p.SetIdleTimeout(d)
		}
	}
	routes, err := routeChain(server, cfg)
	if err != nil {
		return nil, nil, false, err
	}
	if opts.HostKeyPolicy != "" {
		for i := range routes {
			routes[i].key += "|host-key-policy=" + opts.HostKeyPolicy
		}
	}
	var jump *ssh.Client
	keys := make([]string, 0, len(routes))
	created := false
	for _, route := range routes {
		client, wasCreated, err := p.getRouteClient(route, cfg, jump, confirm, opts)
		if err != nil {
			return nil, nil, false, err
		}
		jump = client
		keys = append(keys, route.key)
		created = created || wasCreated
	}
	p.setChainKeys(keys...)
	return jump, keys, created, nil
}

func (p *Pool) SetIdleTimeout(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed && d > 0 {
		p.idleTimeout = d
	}
}

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

func (p *Pool) CloseAll() int {
	p.cancel()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0
	}
	p.closed = true
	entries := p.entries
	p.entries = map[string]*entry{}
	p.mu.Unlock()
	p.closeEntries(entries, true)
	return len(entries)
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
	go func() {
		defer recoverCallbackPanic("ssh pool connect callback")
		p.ConnectCallback(key, client)
	}()
}

func (p *Pool) notifyDisconnect(key string) {
	if p.DisconnectCallback == nil {
		return
	}
	go func() {
		defer recoverCallbackPanic("ssh pool disconnect callback")
		p.DisconnectCallback(key)
	}()
}

func (p *Pool) IsAlive(key string, client *ssh.Client) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	ent, ok := p.entries[key]
	return ok && ent.client == client
}

func recoverCallbackPanic(name string) {
	if r := recover(); r != nil {
		slog.Error(name+" panic", "recover", r)
	}
}

func (p *Pool) getRouteClient(route routeStep, cfg config.RuntimeConfig, jump *ssh.Client, confirm func(HostKeyPrompt) bool, opts DialOptions) (*ssh.Client, bool, error) {
	res, err, shared := p.sf.Do(route.key, func() (any, error) {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("ssh pool is closed")
		}
		if ent := p.entries[route.key]; ent != nil {
			ent.lastAccess = time.Now()
			client := ent.client
			p.mu.Unlock()
			return getClientResult{client: client}, nil
		}
		p.mu.Unlock()

		client, err := dialClient(route.server, cfg, jump, confirm, opts)
		if err != nil {
			return nil, err
		}

		now := time.Now()
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = client.Close()
			return nil, errors.New("ssh pool is closed")
		}
		if ent := p.entries[route.key]; ent != nil {
			ent.lastAccess = now
			existing := ent.client
			p.mu.Unlock()
			_ = client.Close()
			return getClientResult{client: existing}, nil
		}
		p.entries[route.key] = &entry{
			client:     client,
			lastAccess: now,
			serverID:   route.server.ID,
			alias:      route.server.Alias,
			remoteHost: route.server.Host,
			chainKeys:  []string{route.key},
		}
		p.mu.Unlock()

		go p.keepAliveLoop(route.key, client, cfg)
		p.notifyConnect(route.key, client)
		return getClientResult{client: client, isNew: true}, nil
	})
	if err != nil {
		return nil, false, err
	}
	result := res.(getClientResult)
	if shared {
		result.isNew = false
	}
	return result.client, result.isNew, nil
}

func dial(server config.ServerProfile, cfg config.RuntimeConfig, jump *ssh.Client, confirm func(HostKeyPrompt) bool, opts DialOptions) (*ssh.Client, error) {
	auth, closer, err := authMethods(server, cfg, opts)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}
	hostKeyCallback, err := hostKeyCallback(server, confirm, opts.HostKeyPolicy)
	if err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	addr := net.JoinHostPort(server.Host, strconv.Itoa(server.Port))
	conn, err := dialTransport(addr, server, cfg, jump, timeout)
	if err != nil {
		return nil, err
	}
	clientConfig := &ssh.ClientConfig{
		User:            server.User,
		Auth:            auth,
		HostKeyCallback: hostKeyCallback,
		Timeout:         timeout,
	}
	ncc, chans, reqs, err := ssh.NewClientConn(conn, addr, clientConfig)
	if err != nil {
		_ = conn.Close()
		if strings.Contains(err.Error(), "ssh: unable to authenticate") {
			return nil, &AuthError{
				FailedMethod:   server.AuthMethod,
				AllowedMethods: nil,
				Err:            fmt.Errorf("%w: %v", ErrAuthFailed, err),
			}
		}
		return nil, err
	}
	return ssh.NewClient(ncc, chans, reqs), nil
}

func authMethods(server config.ServerProfile, cfg config.RuntimeConfig, opts DialOptions) ([]ssh.AuthMethod, io.Closer, error) {
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
		signer, err := ssh.ParsePrivateKey([]byte(privateKey))
		if err != nil {
			return nil, nil, err
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
		conn, err := dialAgent(socket)
		if err != nil {
			return nil, nil, &AuthError{
				FailedMethod:   config.AuthMethodAgent,
				AllowedMethods: []string{config.AuthMethodPassword, config.AuthMethodKey, config.AuthMethodAgent},
				Err:            fmt.Errorf("%w: connect ssh agent: %v", ErrAuthFailed, err),
			}
		}
		signers, err := agent.NewClient(conn).Signers()
		if err != nil {
			_ = conn.Close()
			return nil, nil, &AuthError{
				FailedMethod:   config.AuthMethodAgent,
				AllowedMethods: []string{config.AuthMethodPassword, config.AuthMethodKey, config.AuthMethodAgent},
				Err:            fmt.Errorf("%w: list ssh agent signers: %v", ErrAuthFailed, err),
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

func dialTransport(addr string, server config.ServerProfile, cfg config.RuntimeConfig, jump *ssh.Client, timeout time.Duration) (net.Conn, error) {
	if jump != nil {
		return jump.Dial("tcp", addr)
	}
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	if server.ProxyID == "" {
		return dialer.Dial("tcp", addr)
	}
	proxyCfg, ok := cfg.Proxies[server.ProxyID]
	if !ok {
		return nil, fmt.Errorf("proxy %s was not found", server.ProxyID)
	}
	proxyAddr := net.JoinHostPort(proxyCfg.Host, strconv.Itoa(proxyCfg.Port))
	switch proxyCfg.Type {
	case config.ProxyTypeSOCKS5:
		var auth *proxy.Auth
		if proxyCfg.Username != "" {
			auth = &proxy.Auth{User: proxyCfg.Username, Password: proxyCfg.Password}
		}
		d, err := proxy.SOCKS5("tcp", proxyAddr, auth, dialer)
		if err != nil {
			return nil, err
		}
		return d.Dial("tcp", addr)
	case config.ProxyTypeHTTP:
		return dialHTTPProxy(proxyAddr, addr, proxyCfg.Username, proxyCfg.Password, dialer)
	default:
		return nil, fmt.Errorf("unsupported proxy type %q", proxyCfg.Type)
	}
}

func dialHTTPProxy(proxyAddr string, targetAddr string, user string, pass string, dialer *net.Dialer) (net.Conn, error) {
	if strings.ContainsAny(targetAddr+user+pass, "\r\n") {
		return nil, errors.New("invalid proxy CONNECT parameters")
	}
	conn, err := dialer.Dial("tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
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

func hostKeyCallback(server config.ServerProfile, confirm func(HostKeyPrompt) bool, policy string) (ssh.HostKeyCallback, error) {
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
				if policy == HostKeyPolicyAsk && confirm != nil && confirm(prompt) {
					return replaceKnownHost(khPath, hostname, key, keyErr.Want)
				}
				return &HostKeyError{Prompt: prompt}
			case policy == HostKeyPolicyAcceptNew:
				return appendKnownHost(khPath, hostname, key)
			case policy == HostKeyPolicyAsk && confirm != nil:
				prompt := newHostKeyPrompt(hostname, key, false)
				if confirm(prompt) {
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
		out = append(out, routeStep{server: jump, key: key})
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

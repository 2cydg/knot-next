package coreclient

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// RuntimeFile follows core's public XDG/legacy-home layout on all platforms.
// An explicit path also supports a daemon started with a custom state directory.
func RuntimeFile() (string, error) {
	return runtimeFile(os.Getenv, os.UserHomeDir, user.Current)
}

func runtimeFile(getenv func(string) string, userHomeDir func() (string, error), currentUser func() (*user.User, error)) (string, error) {
	if p := getenv("KNOT_RUNTIME_FILE"); p != "" {
		if !filepath.IsAbs(p) {
			return "", Failure("INVALID_RUNTIME")
		}
		return p, nil
	}
	base := getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := userHomeDir()
		if err != nil || home == "" {
			usr, userErr := currentUser()
			if userErr != nil || usr == nil || usr.HomeDir == "" {
				return "", Failure("RUNTIME_UNAVAILABLE")
			}
			home = usr.HomeDir
		}
		base = filepath.Join(home, ".local", "state")
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return "", Failure("RUNTIME_UNAVAILABLE")
	}
	return filepath.Join(base, "knot", "runtime", "core.json"), nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, Failure("INVALID_RUNTIME")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, Failure("INVALID_RUNTIME")
	}
	return b, nil
}

func Discover(path string) (*Client, Runtime, error) {
	var runtime Runtime
	b, err := readBounded(path, 64<<10)
	if err != nil {
		if Code(err) == "INVALID_RUNTIME" {
			return nil, runtime, err
		}
		return nil, runtime, Failure("RUNTIME_UNAVAILABLE")
	}
	if json.Unmarshal(b, &runtime) != nil || runtime.InstanceID == "" || runtime.APIVersion != "v1" || runtime.Port < 1 || runtime.Port > 65535 || runtime.TokenPath == "" {
		return nil, runtime, Failure("INVALID_RUNTIME")
	}
	if !filepath.IsAbs(runtime.TokenPath) {
		// Core can publish relative XDG paths. Resolve only its documented
		// sibling-token layout, anchored to the file actually discovered, never
		// to the CLI's (possibly different) working directory.
		if runtime.RuntimePath == "" || filepath.IsAbs(runtime.RuntimePath) || filepath.Base(runtime.RuntimePath) != "core.json" || filepath.Clean(runtime.TokenPath) != filepath.Join(filepath.Dir(runtime.RuntimePath), "token") {
			return nil, runtime, Failure("INVALID_RUNTIME")
		}
		runtime.TokenPath, err = filepath.Abs(filepath.Join(filepath.Dir(path), "token"))
		if err != nil {
			return nil, runtime, Failure("INVALID_RUNTIME")
		}
	}
	token, err := readBounded(runtime.TokenPath, 16<<10)
	if err != nil {
		return nil, runtime, Failure("AUTH_FILE_INVALID")
	}
	for _, address := range runtime.ListenAddresses {
		host := address
		if parsedHost, port, err := net.SplitHostPort(address); err == nil {
			parsedPort, err := strconv.Atoi(port)
			if err != nil || parsedPort != runtime.Port {
				continue
			}
			host = parsedHost
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			continue
		}
		client, err := New("http://"+net.JoinHostPort(host, strconv.Itoa(runtime.Port)), strings.TrimSpace(string(token)))
		return client, runtime, err
	}
	return nil, runtime, Failure("INVALID_RUNTIME")
}

// Load re-discovers credentials on retry and brackets all reads with runtime
// identity checks. A restart cannot combine config from one daemon with another.
func Load(ctx context.Context, path string) (s Snapshot, err error) {
	c, disk, err := Discover(path)
	if err != nil {
		return s, err
	}
	s.Client = c
	ok := false
	defer func() {
		if !ok {
			c.Close()
			s = Snapshot{}
		}
	}()
	s.Runtime, err = c.Runtime(ctx)
	if err != nil {
		return s, err
	}
	if s.Runtime.InstanceID != disk.InstanceID {
		return s, Failure("INSTANCE_CHANGED")
	}
	if s.Runtime.APIVersion != "v1" {
		return s, Failure("API_UNSUPPORTED")
	}
	s.Version, err = c.Version(ctx)
	if err != nil {
		return s, err
	}
	if s.Version.Version == "" || s.Version.APIVersion != "v1" {
		return s, Failure("API_UNSUPPORTED")
	}
	s.Health, err = c.Health(ctx)
	if err != nil {
		return s, err
	}
	if !s.Health.Authenticated {
		return s, Failure("UNAUTHORIZED")
	}
	s.Capabilities, err = c.Capabilities(ctx)
	if err != nil {
		return s, err
	}
	if !s.Available("config") {
		return s, Failure("CAPABILITY_UNAVAILABLE")
	}
	s.Config, err = c.Summary(ctx)
	if err != nil {
		return s, err
	}
	end, err := c.Runtime(ctx)
	if err != nil {
		return s, err
	}
	if end.InstanceID != s.Runtime.InstanceID {
		return s, Failure("INSTANCE_CHANGED")
	}
	ok = true
	return s, nil
}

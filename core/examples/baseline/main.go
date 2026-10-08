// A small public-API workflow client, not a terminal or SDK.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

type client struct {
	base, token string
	http        *http.Client
}
type resource struct {
	ID            string `json:"id"`
	ServerID      string `json:"server_id"`
	State         string `json:"state"`
	Error         string `json:"error"`
	FrameworkCode string `json:"framework_code"`
}

func (c *client) call(ctx context.Context, method, route string, body any, out any) error {
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+route, input)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d", method, route, resp.StatusCode)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return err
	}
	return json.Unmarshal(envelope.Data, out)
}
func (c *client) wait(ctx context.Context, route, ready string, trust bool) error {
	var remembered map[string]any
	for {
		var current resource
		if err := c.call(ctx, "GET", route, nil, &current); err != nil {
			return err
		}
		switch current.State {
		case ready:
			if remembered != nil {
				var profile struct {
					AuthMethod  string `json:"auth_method"`
					PasswordSet bool   `json:"password_set"`
					KeyID       string `json:"key_id"`
				}
				if err := c.call(ctx, "GET", "/v1/config/servers/"+current.ServerID, nil, &profile); err != nil {
					return err
				}
				password, _ := remembered["password"].(string)
				key, _ := remembered["key_id"].(string)
				if (password != "" && (profile.AuthMethod != "password" || !profile.PasswordSet)) || (key != "" && (profile.AuthMethod != "key" || profile.KeyID != key)) {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(50 * time.Millisecond):
						continue
					}
				}
			}
			return nil
		case "failed", "closed", "disconnected":
			return fmt.Errorf("resource ended: %s (%s)", current.State, current.FrameworkCode)
		case "host_key_pending":
			var challenge struct {
				Fingerprint string `json:"fingerprint"`
			}
			if err := c.call(ctx, "GET", route+"/challenges/host-key", nil, &challenge); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "host-key challenge:", challenge.Fingerprint)
			if !trust {
				return fmt.Errorf("host key requires explicit -trust-new: %s", challenge.Fingerprint)
			}
			if err := c.call(ctx, "POST", route+"/challenges/host-key", map[string]any{"accept": true, "remember": true}, nil); err != nil {
				return err
			}
		case "auth_pending":
			var challenge struct {
				PassphraseRequired bool `json:"passphrase_required"`
			}
			if err := c.call(ctx, "GET", route+"/challenges/auth", nil, &challenge); err != nil {
				return err
			}
			body := map[string]any{"remember": true}
			if challenge.PassphraseRequired {
				body["passphrase"] = os.Getenv("KNOT_PASSPHRASE")
			} else if key := os.Getenv("KNOT_KEY_ID"); key != "" {
				body["key_id"] = key
			} else {
				body["password"] = os.Getenv("KNOT_PASSWORD")
			}
			remembered = body
			if err := c.call(ctx, "POST", route+"/challenges/auth", body, nil); err != nil {
				return err
			}
		case "connecting":
		default:
			return fmt.Errorf("unknown session state %q", current.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func (c *client) socket(ctx context.Context, route string) (*websocket.Conn, error) {
	cfg, err := websocket.NewConfig(strings.Replace(c.base, "http://", "ws://", 1)+route, "http://localhost")
	if err != nil {
		return nil, err
	}
	cfg.Header.Set("Authorization", "Bearer "+c.token)
	cfg.Dialer = &net.Dialer{Timeout: 5 * time.Second}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ws, err := cfg.DialContext(dialCtx)
	if err == nil {
		err = ws.SetDeadline(time.Now().Add(10 * time.Second))
		if err != nil {
			ws.Close()
		}
	}
	return ws, err
}
func run(ctx context.Context, c *client, server, source, target string, trust bool) error {
	var shell resource
	if err := c.call(ctx, "POST", "/v1/sessions", map[string]any{"server_ref": server, "allow_auth_retry": true, "host_key_policy": "ask"}, &shell); err != nil {
		return err
	}
	cleanup := func(route string) {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.call(closeCtx, "DELETE", route, nil, nil)
	}
	shellRoute := "/v1/sessions/" + shell.ID
	defer cleanup(shellRoute)
	if err := c.wait(ctx, shellRoute, "connected", trust); err != nil {
		return err
	}
	ws, err := c.socket(ctx, shellRoute+"/attach")
	if err != nil {
		return err
	}
	// The attach snapshot is JSON. This example leaves the terminal untouched.
	var snapshot string
	err = websocket.Message.Receive(ws, &snapshot)
	ws.Close()
	if err != nil {
		return err
	}
	var exec struct {
		State    string `json:"state"`
		ExitCode int    `json:"exit_code"`
	}
	if err = c.call(ctx, "POST", "/v1/sessions/exec", map[string]any{"server_ref": server, "command": "true", "timeout_ms": 5000, "passphrase": os.Getenv("KNOT_PASSPHRASE"), "host_key_policy": "strict"}, &exec); err != nil {
		return err
	}
	if exec.State != "completed" || exec.ExitCode != 0 {
		return errors.New("exec did not complete successfully")
	}
	var files resource
	if err = c.call(ctx, "POST", "/v1/sftp", map[string]any{"server_ref": server, "allow_auth_retry": true, "host_key_policy": "strict"}, &files); err != nil {
		return err
	}
	route := "/v1/sftp/" + files.ID
	defer cleanup(route)
	if err = c.wait(ctx, route, "open", trust); err != nil {
		return err
	}
	var entries any
	if err = c.call(ctx, "GET", route+"/files?path=%2F", nil, &entries); err != nil {
		return err
	}
	var transfer resource
	if err = c.call(ctx, "POST", route+"/upload", map[string]any{"source": source, "target": target, "overwrite": false}, &transfer); err != nil {
		return err
	}
	events, err := c.socket(ctx, route+"/transfers/events")
	if err != nil {
		return err
	}
	var first struct {
		Type      string     `json:"type"`
		Transfers []resource `json:"transfers"`
	}
	err = websocket.JSON.Receive(events, &first)
	events.Close()
	if err != nil {
		return err
	}
	if first.Type != "sftp.transfer.snapshot" {
		return errors.New("missing transfer snapshot")
	}
	// GET is authoritative even if POST completed before subscription or WS broke.
	for {
		var current resource
		if err = c.call(ctx, "GET", route+"/transfers/"+transfer.ID, nil, &current); err != nil {
			return err
		}
		switch current.State {
		case "completed":
			return nil
		case "failed", "partial_failed", "canceled":
			return fmt.Errorf("transfer ended: %s", current.State)
		case "pending", "running", "queued":
		default:
			return fmt.Errorf("unknown transfer state %q", current.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func main() {
	home, _ := os.UserHomeDir()
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(home, ".local", "state")
	}
	runtimePath := flag.String("runtime", filepath.Join(state, "knot", "runtime", "core.json"), "daemon runtime file")
	server := flag.String("server", "", "existing server ID or alias")
	source := flag.String("source", "", "local upload file")
	target := flag.String("target", "", "new remote file path")
	trust := flag.Bool("trust-new", false, "accept displayed unknown host key for this workflow")
	flag.Parse()
	if *server == "" || *source == "" || *target == "" {
		fmt.Fprintln(os.Stderr, "-server, -source and -target are required")
		os.Exit(2)
	}
	err := func() error {
		raw, err := os.ReadFile(*runtimePath)
		if err != nil {
			return err
		}
		var runtime struct {
			ListenAddresses []string `json:"listen_addresses"`
			TokenPath       string   `json:"token_path"`
		}
		if err = json.Unmarshal(raw, &runtime); err != nil {
			return err
		}
		if len(runtime.ListenAddresses) == 0 {
			return errors.New("runtime has no listener")
		}
		token, err := os.ReadFile(runtime.TokenPath)
		if err != nil {
			return err
		}
		c := &client{base: "http://" + runtime.ListenAddresses[0], token: strings.TrimSpace(string(token)), http: &http.Client{Timeout: 15 * time.Second}}
		defer c.http.CloseIdleConnections()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return run(ctx, c, *server, *source, *target, *trust)
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("SSH attach/exec, SFTP listing/upload, snapshot/GET and resource close completed")
}

package session

import (
	"io"
	"strings"
	"testing"
	"time"

	"knot-core/pkg/config"
)

type testConfigService struct {
	runtime config.RuntimeConfig
}

func (s *testConfigService) RuntimeConfig() (config.RuntimeConfig, error) {
	return cloneRuntimeConfig(s.runtime), nil
}

func (s *testConfigService) SetServerPassword(id string, password string) (config.ServerProfileView, error) {
	server := s.runtime.Servers[id]
	server.Password = password
	server.AuthMethod = config.AuthMethodPassword
	s.runtime.Servers[id] = server
	return config.ServerProfileView{ID: id, PasswordSet: password != ""}, nil
}

func (s *testConfigService) UpdateServer(id string, server config.ServerProfile) (config.ServerProfileView, error) {
	s.runtime.Servers[id] = server
	return config.ServerProfileView{ID: id, KeyID: server.KeyID, AuthMethod: server.AuthMethod}, nil
}

func TestListWithOptions(t *testing.T) {
	svc := NewService()
	svc.UseLocalTestBackend()
	var ids []string
	for _, req := range []CreateRequest{
		{ServerRef: "srv-1", Alias: "alpha"},
		{ServerRef: "srv-1", Alias: "beta"},
		{ServerRef: "srv-2", Alias: "gamma"},
	} {
		res, err := svc.Create(req)
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		ids = append(ids, res.ID)
	}
	if _, err := svc.Disconnect(ids[1]); err != nil {
		t.Fatalf("disconnect session: %v", err)
	}
	if got := len(svc.ListWithOptions(ListOptions{ServerRef: "srv-1"})); got != 2 {
		t.Fatalf("server filter count = %d, want 2", got)
	}
	if got := len(svc.ListWithOptions(ListOptions{Alias: "gamma"})); got != 1 {
		t.Fatalf("alias filter count = %d, want 1", got)
	}
	if got := len(svc.ListWithOptions(ListOptions{State: "closed"})); got != 1 {
		t.Fatalf("state filter count = %d, want 1", got)
	}
}

func TestSubscribeCWDReceivesUpdates(t *testing.T) {
	svc := NewService()
	svc.UseLocalTestBackend()
	res, err := svc.Create(CreateRequest{ServerRef: "srv-1", Alias: "alpha"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	ch, cancel, _, err := svc.SubscribeCWD(res.ID)
	if err != nil {
		t.Fatalf("subscribe cwd: %v", err)
	}
	defer cancel()
	svc.updateCurrentDir(res.ID, "/var/www")
	select {
	case notify := <-ch:
		if notify.Path != "/var/www" {
			t.Fatalf("path = %q, want /var/www", notify.Path)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cwd notify")
	}
}

func TestOSC7ParserObserve(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{name: "bel", input: []string{"x\x1b]7;file://host/var/www\a"}, want: []string{"/var/www"}},
		{name: "st", input: []string{"\x1b]7;file://host/tmp/a%20b\x1b\\"}, want: []string{"/tmp/a b"}},
		{name: "split", input: []string{"abc\x1b]7;file://host", "/srv/app\a"}, want: []string{"/srv/app"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p osc7Parser
			var got []string
			for _, chunk := range tt.input {
				_, paths, _ := p.Observe([]byte(chunk))
				got = append(got, paths...)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("paths = %#v, want %#v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("paths = %#v, want %#v", got, tt.want)
				}
			}
		})
	}
}

func TestRuntimeConfigForAuthResponse(t *testing.T) {
	svc := NewService()
	cfgSvc := &testConfigService{
		runtime: config.RuntimeConfig{
			Servers: map[string]config.ServerProfile{
				"srv": {ID: "srv", Alias: "web", AuthMethod: config.AuthMethodAgent},
			},
			Proxies:       map[string]config.ProxyProfile{},
			Keys:          map[string]config.KeyMetadata{},
			SyncProviders: map[string]config.SyncProviderConfig{},
		},
	}
	svc.UseConfig(cfgSvc)
	next, err := svc.runtimeConfigForAuthResponse(cfgSvc.runtime, cfgSvc.runtime.Servers["srv"], ChallengeResponse{
		Password: "secret",
		Remember: true,
	})
	if err != nil {
		t.Fatalf("runtimeConfigForAuthResponse: %v", err)
	}
	if got := next.Servers["srv"].Password; got != "secret" {
		t.Fatalf("password = %q, want secret", got)
	}
	if got := next.Servers["srv"].AuthMethod; got != config.AuthMethodPassword {
		t.Fatalf("auth method = %q, want password", got)
	}
}

func TestChallengeResponseResolvesPendingState(t *testing.T) {
	svc := NewService()
	svc.UseLocalTestBackend()
	res, err := svc.Create(CreateRequest{ServerRef: "srv"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := time.Now().UTC()
	respCh := make(chan ChallengeResponse, 1)
	svc.mu.Lock()
	current := svc.sessions[res.ID]
	current.hostKeyChallenge = &pendingChallenge{
		Challenge: Challenge{SessionID: res.ID, Type: "host_key", Pending: true, CreatedAt: now, UpdatedAt: now},
		response:  respCh,
	}
	current.HostKeyPending = true
	current.State = "host_key_pending"
	svc.mu.Unlock()
	challenge, err := svc.RespondHostKeyChallenge(res.ID, ChallengeResponse{Accept: true})
	if err != nil {
		t.Fatalf("respond host key: %v", err)
	}
	if challenge.Pending {
		t.Fatal("challenge should no longer be pending")
	}
	updated, err := svc.Get(res.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if updated.HostKeyPending {
		t.Fatal("host key pending should be false")
	}
}

func TestObservedReaderReportsPaths(t *testing.T) {
	paths := make(chan string, 1)
	reader := newObservedReader(strings.NewReader("\x1b]7;file://host/tmp\a"), func(path string) {
		paths <- path
	})
	buf := make([]byte, 64)
	if _, err := reader.Read(buf); err != nil && err != io.EOF {
		t.Fatalf("read observed reader: %v", err)
	}
	select {
	case path := <-paths:
		if path != "/tmp" {
			t.Fatalf("path = %q, want /tmp", path)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for observed path")
	}
}

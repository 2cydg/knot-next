package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	protocol "github.com/pkg/sftp"
	api "knot-core/internal/api/http"
	"knot-core/internal/auth"
	"knot-core/internal/paths"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/core"
	"knot-core/pkg/crypto"
	"knot-core/pkg/secret"
	"knot-core/pkg/session"
	files "knot-core/pkg/sftp"
	"knot-core/pkg/sshpool"
)

func TestBaselineWorkflowThroughPublicAPI(t *testing.T) {
	handlers := protocol.InMemHandler()
	remote := sshserver.New(t, sshserver.Config{User: "test", Password: "example-password", SFTPHandlers: &handlers})
	remote.SetExecBehavior(sshserver.ExecBehavior{Scripted: true, SendExitStatus: true})
	layout := paths.NewLayout(filepath.Join(t.TempDir(), "cfg"), filepath.Join(t.TempDir(), "state"))
	provider := crypto.NewStaticProvider([]byte("example-test"))
	cfg := config.NewService(layout, provider)
	pool := sshpool.NewPool()
	sessions := session.NewService()
	sessions.UseConfig(cfg)
	sessions.UsePool(pool)
	sftp := files.NewService(t.TempDir())
	sftp.UseConfig(cfg)
	sftp.UsePool(pool)
	service := core.New("example", time.Now())
	service.UseConfig(cfg)
	service.UseSecret(secret.NewService(cfg, provider))
	service.UseSession(sessions)
	service.UseSFTP(sftp)
	service.UseSSHPool(pool)
	tracker := api.NewConnTracker()
	server := httptest.NewServer(api.NewServer(service, nil, auth.NewVerifier("test-token"), auth.NewOriginChecker([]string{"http://localhost"}), api.WithConnTracker(tracker)).Handler())
	defer func() {
		tracker.CloseAll()
		server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	c := &client{base: server.URL, token: "test-token", http: server.Client()}
	c.http.Timeout = 15 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var profile resource
	if err := c.call(ctx, "POST", "/v1/config/servers", map[string]any{"alias": "example", "host": remote.Host(), "port": remote.Port(), "user": "test", "auth_method": "password"}, &profile); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KNOT_PASSWORD", "example-password")
	source := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(source, []byte("example\x00\xff"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, c, profile.ID, source, "/example-upload", true); err != nil {
		t.Fatal(err)
	}

	var shells []resource
	if err := c.call(ctx, "GET", "/v1/sessions", nil, &shells); err != nil {
		t.Fatal(err)
	}
	for _, r := range shells {
		if r.State != "closed" {
			t.Errorf("resource was not closed: %+v", r)
		}
	}
	ftp, err := service.SFTP().Get("1")
	if err != nil || ftp.State != "closed" {
		t.Fatalf("SFTP cleanup: %+v %v", ftp, err)
	}
}

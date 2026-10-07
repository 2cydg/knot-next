package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	apihttp "knot-core/internal/api/http"
	"knot-core/internal/auth"
	"knot-core/internal/paths"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/core"
	"knot-core/pkg/crypto"
	"knot-core/pkg/secret"
	"knot-core/pkg/session"
	"knot-core/pkg/sftp"
	"knot-core/pkg/sshpool"

	protocolsftp "github.com/pkg/sftp"
)

// Only this fixture imports business services to assemble the real API. The
// client and scenarios use HTTP/WS JSON exclusively and never enable testMode.
// Handler interfaces are captured by value at startup; configure them before
// calling this function. Their referenced implementations are shared by all
// subsystems on the remote, so both sessions see the same remote filesystem.
// This fixture does not wire process context, API shutdown, or runtime discovery;
// it covers resource API behavior, not process-level shutdown of in-flight work.
func newSFTPAPI(t *testing.T, handlers protocolsftp.Handlers) (*sftpAPIClient, *sshserver.Server) {
	t.Helper()
	remote := sshserver.New(t, sshserver.Config{
		User: "sftp-test", Password: "artificial-password", SFTPHandlers: &handlers,
	})
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "config"), filepath.Join(root, "state"))
	provider := crypto.NewStaticProvider([]byte("sftp-integration-key"))
	cfg := config.NewService(layout, provider)
	pool := sshpool.NewPool()
	sshSessions := session.NewService()
	sshSessions.UseConfig(cfg)
	sshSessions.UsePool(pool)
	files := sftp.NewService(filepath.Join(root, "sftp"))
	files.UseConfig(cfg)
	files.UsePool(pool)
	files.UseSession(sshSessions)
	service := core.New("sftp-integration", time.Now())
	service.UseConfig(cfg)
	service.UseSecret(secret.NewService(cfg, provider))
	service.UseSession(sshSessions)
	service.UseSFTP(files)
	service.UseSSHPool(pool)
	tracker := apihttp.NewConnTracker()
	api := apihttp.NewServer(service, nil, auth.NewVerifier("sftp-test-token"), auth.NewOriginChecker([]string{sftpTestOrigin}), apihttp.WithConnTracker(tracker))
	httpServer := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		tracker.CloseAll()
		httpServer.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Errorf("release SFTP fixture: %v", err)
		}
	})
	client := &sftpAPIClient{baseURL: httpServer.URL, token: "sftp-test-token", http: &http.Client{Timeout: 5 * time.Second}}
	t.Cleanup(client.http.CloseIdleConnections)
	var profile struct {
		ID string `json:"id"`
	}
	client.call(t, http.MethodPost, "/v1/config/servers", map[string]any{
		"alias": "controlled-sftp", "host": remote.Host(), "port": remote.Port(),
		"user": "sftp-test", "auth_method": "password",
	}, http.StatusCreated, &profile)
	client.call(t, http.MethodPut, "/v1/secrets/servers/"+profile.ID+"/password",
		map[string]any{"password": "artificial-password"}, http.StatusOK, nil)
	client.serverRef = profile.ID
	return client, remote
}

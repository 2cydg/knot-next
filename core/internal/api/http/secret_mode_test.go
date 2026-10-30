package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"knot-core/internal/auth"
	coreruntime "knot-core/internal/runtime"
	"knot-core/pkg/core"
)

func TestSecretAuthenticationAppliesToEveryAPIAndWebSocket(t *testing.T) {
	for _, secret := range []string{"", "configured-secret"} {
		server := NewServer(core.New(core.DefaultVersion, time.Now()), coreruntime.NewHolder(), auth.NewVerifier(secret), auth.NewOriginChecker(nil))
		for _, path := range []string{"/v1/version", "/v1/runtime", "/v1/health", "/v1/status", "/v1/config", "/v1/events", "/v1/sessions/id/attach"} {
			for _, credential := range []string{"", "wrong", "configured-secret"} {
				request := httptest.NewRequest(http.MethodGet, path, nil)
				if credential != "" {
					request.Header.Set("Authorization", "Bearer "+credential)
				}
				recorder := httptest.NewRecorder()
				server.Handler().ServeHTTP(recorder, request)
				expectedUnauthorized := secret != "" && credential != secret
				if (recorder.Code == 401) != expectedUnauthorized {
					t.Fatalf("secret configured=%v path=%s credential correct=%v status=%d", secret != "", path, credential == secret, recorder.Code)
				}
				if strings.Contains(recorder.Body.String(), "configured-secret") {
					t.Fatal("API leaked Secret")
				}
			}
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Origin", "http://untrusted.test")
			request.Header.Set("Authorization", "Bearer "+secret)
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if recorder.Code != 403 {
				t.Fatal("Origin check bypassed in Secret mode", secret != "", path, recorder.Code)
			}
		}
	}
}

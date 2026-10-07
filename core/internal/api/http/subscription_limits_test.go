package http

import (
	"context"
	"encoding/json"
	"knot-core/pkg/session"
	"knot-core/pkg/sftp"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResourceSubscriberLimitsReturnConflict(t *testing.T) {
	server := newStatefulTestServer(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.core.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	ssh, err := server.core.Session().Create(session.CreateRequest{ServerRef: "test"})
	if err != nil {
		t.Fatal(err)
	}
	files, err := server.core.SFTP().Create(sftp.CreateRequest{ServerRef: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, route, code string
		limit             int
		subscribe         func() (func(), error)
	}{
		{"global", "/v1/events", "EVENT_SUBSCRIPTION_UNAVAILABLE", 64, func() (func(), error) { _, cancel, err := server.core.SubscribeEvents(); return cancel, err }},
		{"ssh", "/v1/sessions/" + ssh.ID + "/events", "CONFLICT", 16, func() (func(), error) {
			_, cancel, _, err := server.core.Session().Subscribe(ssh.ID)
			return cancel, err
		}},
		{"sftp", "/v1/sftp/" + files.ID + "/events", "CONFLICT", 16, func() (func(), error) {
			_, cancel, _, err := server.core.SFTP().Subscribe(files.ID)
			return cancel, err
		}},
		{"transfers", "/v1/sftp/" + files.ID + "/transfers/events", "CONFLICT", 16, func() (func(), error) {
			_, cancel, err := server.core.SFTP().SubscribeTransfers(files.ID)
			return cancel, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cancels []func()
			defer func() {
				for _, cancel := range cancels {
					cancel()
					cancel()
				}
			}()
			for i := 0; i < tc.limit; i++ {
				cancel, err := tc.subscribe()
				if err != nil {
					t.Fatal(err)
				}
				cancels = append(cancels, cancel)
			}
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, authenticatedRequest(http.MethodGet, tc.route))
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusConflict || envelope.Error.Code != tc.code {
				t.Fatalf("response=%d %s", rec.Code, rec.Body.String())
			}
			cancels[0]()
			cancels[0]()
			cancel, err := tc.subscribe()
			if err != nil {
				t.Fatalf("cancel did not free capacity: %v", err)
			}
			cancel()
		})
	}
}

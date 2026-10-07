package sftp

import (
	"knot-core/pkg/config"
	"strings"
	"testing"
	"time"

	"knot-core/pkg/session"
)

// TestReviewShortCredentialIsRedacted covers RR07 on the SFTP path: the same
// sanitizer must remove every non-empty secret, however short, so a config
// writer that echoes a one- or two-character password back cannot publish it.
func TestReviewShortCredentialIsRedacted(t *testing.T) {
	cases := []struct {
		name    string
		message string
		secret  string
	}{
		{"one character", "cannot save password=a", "a"},
		{"two characters", "cannot save password=xy", "xy"},
		{"attached context", "writer rejected password=xy_suffix", "xy"},
		{"three characters", "cannot save password=xyz", "xyz"},
		{"multi-byte", "cannot save password=密码", "密码"},
		{"ordinary length", "cannot save password=hunter2-secret", "hunter2-secret"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := NewService(t.TempDir())
			service.sessions["fake"] = &resource{Session: Session{ID: "fake", State: "open"}}

			events, cancel, _, err := service.Subscribe("fake")
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			defer cancel()

			service.publishWarning("fake", session.WarningCredentialSaveFailed, tc.message, tc.secret)

			var warning *session.Warning
			deadline := time.After(5 * time.Second)
			for warning == nil {
				select {
				case event := <-events:
					warning = event.Warning
				case <-deadline:
					t.Fatal("the warning was never delivered")
				}
			}
			if strings.Contains(warning.Message, "password="+tc.secret) {
				t.Fatalf("credential leaked in warning: %q", warning.Message)
			}
			if !strings.Contains(warning.Message, redacted) {
				t.Fatalf("warning %q has no redaction marker", warning.Message)
			}
		})
	}
}

func TestReviewRedactionRemovesEmbeddedShortSecret(t *testing.T) {
	got := sanitizeWarning("writer rejected password=xy_suffix and candidate_prefixxy", "xy")
	if strings.Contains(got, "xy") {
		t.Fatalf("short secret leaked: %s", got)
	}
}

func TestReviewSaveWarningUsesFixedMessage(t *testing.T) {
	for _, password := range []string{"a", "xy", "密码", "long-artificial-secret"} {
		t.Run(password, func(t *testing.T) {
			cfg := newRecordingConfigProvider(config.RuntimeConfig{})
			cfg.failSavesWith("arbitrary-writer-error-prefix")
			svc := NewService(t.TempDir())
			svc.UseConfig(cfg)
			svc.sessions["fake"] = &resource{Session: Session{ID: "fake", State: "open"}}
			events, cancel, _, err := svc.Subscribe("fake")
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			svc.saveCredentials("fake", "fake", ChallengeResponse{Password: password, Remember: true})
			select {
			case event := <-events:
				if event.Warning == nil || event.Warning.Message != "password was not saved" {
					t.Fatalf("event=%+v", event)
				}
			case <-time.After(time.Second):
				t.Fatal("no warning")
			}
		})
	}
}

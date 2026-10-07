package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLogsRedactAllSecretSentinels(t *testing.T) {
	file, err := Open(filepath.Join(t.TempDir(), "logs", "core.log"), Options{Level: slog.LevelDebug})
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{"TOKEN_SENTINEL_482", "PASSWORD_SENTINEL_937", "PASSPHRASE_SENTINEL_871", "PRIVATE_SENTINEL_335"}
	for _, secret := range secrets {
		file.Redactor().Add(secret)
	}
	nested := file.Logger().With("instance_id", "instance-1").WithGroup("resource").With("id", "ssh-1").WithGroup("failure")
	nested.Debug(strings.Join(secrets, " "), "wrapped", fmt.Errorf("outer: %w", errors.New(strings.Join(secrets, " "))), "object", map[string]any{"password": "unknown_password_value", "authorization": "Bearer unknown_token_value", "nested": map[string]any{"message": secrets[2]}}, "url", "http://proxy-user:proxy-password@host/path")
	nested.Error("Authorization: Bearer HEADER_SENTINEL password=ERROR_PASSWORD_SENTINEL passphrase=ERROR_PASSPHRASE_SENTINEL")
	file.Logger().Error("-----BEGIN OPENSSH PRIVATE KEY-----\nPEM_SENTINEL\n-----END OPENSSH PRIVATE KEY-----")
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range append(secrets, "unknown_password_value", "unknown_token_value", "proxy-password", "HEADER_SENTINEL", "ERROR_PASSWORD_SENTINEL", "ERROR_PASSPHRASE_SENTINEL", "PEM_SENTINEL") {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("leaked %q", secret)
		}
	}
	var first map[string]any
	if err = json.Unmarshal(bytes.Split(raw, []byte{'\n'})[0], &first); err != nil {
		t.Fatal(err)
	}
	resource := first["resource"].(map[string]any)
	if first["instance_id"] != "instance-1" || resource["id"] != "ssh-1" || resource["failure"] == nil {
		t.Fatalf("lost slog group/With semantics: %s", raw)
	}
}
func TestRotationBoundsHistoryAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "core.log")
	for restart := 0; restart < 2; restart++ {
		file, err := Open(path, Options{MaxBytes: 256, History: 3})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 40; j++ {
					file.Logger().Info("rotation", "payload", strings.Repeat("x", 90))
				}
			}()
		}
		wg.Wait()
		file.Logger().Info(strings.Repeat("oversized", 100))
		if err = file.Close(); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) > 4 {
			t.Fatal("unbounded history")
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() > 256 {
				t.Fatalf("oversized file %s: %d", entry.Name(), info.Size())
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatal("unsafe file permissions")
			}
		}
	}
}
func TestConcurrentCloseAndWriteFailure(t *testing.T) {
	file, err := Open(filepath.Join(t.TempDir(), "core.log"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				file.Logger().Info("write")
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); _ = file.Close() }()
	wg.Wait()
	if _, err = file.Write([]byte("late")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("late write: %v", err)
	}
	failed, err := Open(filepath.Join(t.TempDir(), "core.log"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = failed.file.Close()
	if _, err = failed.Write([]byte("failure")); err == nil || failed.Err() == nil {
		t.Fatal("write failure not observable")
	}
	if err = failed.Close(); err == nil {
		t.Fatal("Close concealed failed writer")
	}
	if _, err = Open(t.TempDir(), Options{}); err == nil {
		t.Fatal("accepted directory as log file")
	}
}
func TestRedactorRegistrationAndSaturation(t *testing.T) {
	r := NewRedactor()
	r.Register(struct {
		Password string
		Nested   map[string]any
	}{"registered_secret", map[string]any{"passphrase": "nested_secret"}})
	if out := r.Text("registered_secret nested_secret"); out != Redacted+" "+Redacted {
		t.Fatal(out)
	}
	r.Add(strings.Repeat("secret", 1<<20))
	if r.Text("anything") != Redacted {
		t.Fatal("saturation did not fail closed")
	}
	var buf bytes.Buffer
	h := &redactingHandler{next: slog.NewJSONHandler(&buf, nil), r: r}
	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "message", 0)); err != nil {
		t.Fatal(err)
	}
}

func TestRotationFailureIsObservableAndOriginalRetained(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.log")
	file, err := Open(path, Options{MaxBytes: 128, History: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte(strings.Repeat("x", 100))); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path+".1", 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(path+".1", "keep"), []byte("do not remove"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte(strings.Repeat("y", 100))); err == nil {
		t.Fatal("rotation error ignored")
	}
	if file.Err() == nil || file.Close() == nil {
		t.Fatal("rotation failure hidden")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != strings.Repeat("x", 100) {
		t.Fatal("rotation failure damaged original log")
	}
}
func TestDiagnosticPreservesMinimumTrailAtErrorLevel(t *testing.T) {
	file, err := Open(filepath.Join(t.TempDir(), "core.log"), Options{Level: slog.LevelError})
	if err != nil {
		t.Fatal(err)
	}
	previous := slog.Default()
	slog.SetDefault(file.Logger())
	defer slog.SetDefault(previous)
	file.Logger().Info("ordinary-info-suppressed")
	Diagnostic(slog.LevelInfo, "core.ready", "instance_id", "test-instance")
	Diagnostic(slog.LevelWarn, "resource-failed", "reason", "connection_refused")
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(file.path)
	if bytes.Contains(raw, []byte("ordinary-info-suppressed")) || !bytes.Contains(raw, []byte("core.ready")) || !bytes.Contains(raw, []byte("connection_refused")) {
		t.Fatalf("level behavior: %s", raw)
	}
}

func TestNestedSlogGroupsAndLogValuerAreRedacted(t *testing.T) {
	file, err := Open(filepath.Join(t.TempDir(), "core.log"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	file.Redactor().Add("nested_sentinel")
	file.Logger().WithGroup("").Info("groups", slog.Group("nested", slog.String("password", "unregistered_value"), slog.String("detail", "nested_sentinel")), "int", 7, "nil", nil, "unmarshalable", make(chan int))
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(file.path)
	if bytes.Contains(raw, []byte("unregistered_value")) || bytes.Contains(raw, []byte("nested_sentinel")) {
		t.Fatal("nested group leaked secret")
	}
}

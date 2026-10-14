package integration

import (
	"bytes"
	protocolsftp "github.com/pkg/sftp"
	"knot-core/pkg/sftp"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestLiteralSymlinkRenameThroughPublicSFTPAPI(t *testing.T) {
	handlers := protocolsftp.InMemHandler()
	// Seed only the remote fixture, through the protocol handler's public API.
	// Every operation under test goes through core HTTP and real SFTP packets.
	if err := handlers.FileCmd.Filecmd(&protocolsftp.Request{Method: "Mkdir", Filepath: "/target-dir"}); err != nil {
		t.Fatal(err)
	}
	if err := handlers.FileCmd.Filecmd(&protocolsftp.Request{Method: "Symlink", Filepath: "/target-dir", Target: "/link-dir"}); err != nil {
		t.Fatal(err)
	}
	c, _ := newSFTPAPI(t, handlers)
	var files sftp.Session
	c.call(t, http.MethodPost, "/v1/sftp", map[string]any{"server_ref": c.serverRef, "host_key_policy": "insecure-skip"}, 201, &files)
	route := "/v1/sftp/" + files.ID
	waitCWD(t, c, route, func(r cwdSessionJSON) bool { return r.State == "open" })
	local := filepath.Join(t.TempDir(), "input")
	payload := []byte("selected literal \x00\xff")
	if err := os.WriteFile(local, payload, 0600); err != nil {
		t.Fatal(err)
	}
	for _, remote := range []string{"/[a].txt", "/a.txt"} {
		var transfer transferJSON
		c.call(t, http.MethodPost, route+"/upload", map[string]any{"source": local, "target": remote}, 202, &transfer)
		if result := c.waitTransfer(t, files.ID, transfer.ID); result.State != "completed" {
			t.Fatal(result)
		}
	}
	target := t.TempDir()
	var download transferJSON
	c.call(t, http.MethodPost, route+"/batch-download", map[string]any{"sources": []string{"/[a].txt"}, "target": target, "source_mode": "literal"}, 202, &download)
	if result := c.waitTransfer(t, files.ID, download.ID); result.State != "completed" {
		t.Fatal(result)
	}
	got, err := os.ReadFile(filepath.Join(target, "[a].txt"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("literal filename changed", err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 1 {
		t.Fatal("literal glob-expanded")
	}
	c.call(t, http.MethodPost, route+"/rename", map[string]any{"old_path": "/[a].txt", "new_path": "/a.txt", "overwrite": false}, 409, nil)
	c.call(t, http.MethodPost, route+"/rename", map[string]any{"old_path": "/[a].txt", "new_path": "/renamed.txt", "overwrite": false}, 200, nil)
	var entry sftp.Entry
	c.call(t, http.MethodGet, route+"/files?path=%2Flink-dir&stat=true&follow_symlinks=false", nil, 200, &entry)
	if entry.Type != "symlink" {
		t.Fatal("Lstat followed link", entry)
	}
	c.call(t, http.MethodGet, route+"/files?path=%2Flink-dir&stat=true", nil, 200, &entry)
	if entry.Type != "directory" {
		t.Fatal("Stat did not follow link", entry)
	}
	c.call(t, http.MethodDelete, route+"/dirs?path=%2Flink-dir", nil, 400, nil)
	c.call(t, http.MethodDelete, route+"/files?path=%2Flink-dir", nil, 200, nil)
	c.call(t, http.MethodGet, route+"/files?path=%2Ftarget-dir&stat=true", nil, 200, &entry)
	if entry.Type != "directory" {
		t.Fatal("link removal touched target")
	}
}

package sftp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiteralDownloadSymlinkAndNoReplace(t *testing.T) {
	s := NewService(t.TempDir())
	s.UseLocalTestBackend()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	res, err := s.Create(CreateRequest{ServerRef: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(res.Root, "[a].txt")
	if err = os.WriteFile(selected, []byte("literal selected"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(res.Root, "a.txt"), []byte("must not expand"), 0600); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	transfer, err := s.BatchDownload(res.ID, BatchTransferRequest{Sources: []string{"/[a].txt"}, SourceMode: "literal", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		transfer, err = s.Transfer(res.ID, transfer.ID)
		if err != nil {
			t.Fatal(err)
		}
		if isTerminalState(transfer.State) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("transfer not finished")
		}
		time.Sleep(time.Millisecond)
	}
	if transfer.State != "completed" {
		t.Fatal(transfer)
	}
	files, err := os.ReadDir(target)
	if err != nil || len(files) != 1 || files[0].Name() != "[a].txt" {
		t.Fatalf("literal expanded: %+v %v", files, err)
	}
	content, _ := os.ReadFile(filepath.Join(target, "[a].txt"))
	if !bytes.Equal(content, []byte("literal selected")) {
		t.Fatal("wrong download")
	}
	if _, err = s.RenameWithOptions(res.ID, "/[a].txt", "/a.txt", false); err == nil {
		t.Fatal("overwrite=false replaced destination")
	}
	if _, err = s.RenameWithOptions(res.ID, "/[a].txt", "/renamed", false); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(res.Root, "link")
	if err = os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	entry, err := s.Lstat(res.ID, "/link")
	if err != nil || entry.Type != "symlink" {
		t.Fatalf("lstat: %+v %v", entry, err)
	}
	if err = s.RemoveDir(res.ID, "/link"); err == nil {
		t.Fatal("symlink treated as directory")
	}
	if err = s.RemoveFile(res.ID, "/link"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(outside); err != nil {
		t.Fatal("link target removed")
	}
}
func TestDescendingSortEqualKeysDeterministic(t *testing.T) {
	input := []Entry{{Name: "b", Path: "/b", Size: 7}, {Name: "a", Path: "/a", Size: 7}, {Name: "c", Path: "/c", Size: 1}, {Name: "a", Path: "/a", Size: 7}}
	sortEntries(input, "-size")
	if input[0].Name != "b" || input[1].Name != "a" || input[2].Name != "a" || input[3].Name != "c" {
		t.Fatal(input)
	}
	sortEntries(input, "size")
	if input[0].Name != "c" || input[3].Name != "b" {
		t.Fatal(input)
	}
}

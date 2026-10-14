package keyutil

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadSourceBoundsRegularFilesAndPreservesOriginalBytes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "key")
	for _, size := range []int{64, MaxSourceBytes, MaxSourceBytes + 1} {
		raw := bytes.Repeat([]byte{'\r', '\n', 0, 255}, (size+3)/4)[:size]
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadSource(file)
		if size > MaxSourceBytes {
			if !errors.Is(err, ErrSourceTooLarge) || got != nil {
				t.Fatal("oversized source accepted", err)
			}
		} else if err != nil || !bytes.Equal(got, raw) {
			t.Fatal("original bytes changed", size, err)
		}
	}
	if _, err := ReadSource(t.TempDir()); !errors.Is(err, ErrSourceNotRegular) {
		t.Fatal(err)
	}
	if _, err := ReadSource(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrSourceUnreadable) {
		t.Fatal(err)
	}
}

func TestReadLegacySourceRetainsRelativePathCompatibility(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	raw := []byte("original\r\nbytes\x00\xff")
	if err := os.WriteFile("key", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSource("key"); !errors.Is(err, ErrSourcePathNotAbsolute) {
		t.Fatal("new import accepted relative path", err)
	}
	got, err := ReadLegacySource("key")
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("legacy relative path changed", err)
	}
}

package keyutil

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

const MaxSourceBytes = 1 << 20

var (
	ErrSourcePathNotAbsolute = errors.New("source_path must be absolute")
	ErrSourceUnreadable      = errors.New("private key source cannot be read")
	ErrSourceNotRegular      = errors.New("private key source must be a regular file")
	ErrSourceTooLarge        = errors.New("private key exceeds 1 MiB")
)

// ReadSource returns original bytes from an absolute, regular private-key file.
// The preliminary stat avoids opening ordinary devices or FIFOs; the opened
// descriptor is checked again and reads remain bounded if the file grows.
func ReadSource(sourcePath string) ([]byte, error) {
	if !filepath.IsAbs(sourcePath) {
		return nil, ErrSourcePathNotAbsolute
	}
	pre, err := os.Stat(sourcePath)
	if err != nil {
		return nil, ErrSourceUnreadable
	}
	if !pre.Mode().IsRegular() {
		return nil, ErrSourceNotRegular
	}
	f, err := os.Open(sourcePath)
	if err != nil {
		return nil, ErrSourceUnreadable
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, ErrSourceUnreadable
	}
	if !info.Mode().IsRegular() {
		return nil, ErrSourceNotRegular
	}
	if info.Size() > MaxSourceBytes {
		return nil, ErrSourceTooLarge
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxSourceBytes+1))
	if err != nil {
		return nil, ErrSourceUnreadable
	}
	if len(raw) > MaxSourceBytes {
		return nil, ErrSourceTooLarge
	}
	return raw, nil
}

// ReadLegacySource preserves historical relative-path resolution against core's
// working directory. All actual file validation and I/O use ReadSource.
func ReadLegacySource(sourcePath string) ([]byte, error) {
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		return nil, ErrSourceUnreadable
	}
	return ReadSource(abs)
}

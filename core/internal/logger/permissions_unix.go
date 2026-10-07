//go:build !windows

package logger

import "os"

func secureDirectory(path string) error { return os.Chmod(path, 0700) }
func secureFile(file *os.File) error    { return file.Chmod(0600) }

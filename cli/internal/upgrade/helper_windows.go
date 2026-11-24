package upgrade

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"

	"knot-cli/internal/corestart"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
)

const helperArgument = "--internal-upgrade-helper"

func startHelper(tx transaction, tag string, console commandIO) error {
	self := windows.CurrentProcess()
	var parent windows.Handle
	if err := windows.DuplicateHandle(self, self, self, &parent, windows.SYNCHRONIZE, true, 0); err != nil {
		return err
	}
	defer windows.CloseHandle(parent)
	// Run the old CLI from its backup path. It understands the same handoff
	// protocol and leaves both installed paths free for replacement.
	cmd := exec.Command(filepath.Join(tx.backup, binaryNames[1]), helperArgument,
		strconv.FormatUint(uint64(parent), 10), string(console.tr.Language()), tag, filepath.Base(tx.stage))
	cmd.Stdout, cmd.Stderr = console.out, console.diagnostics
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags:              windows.CREATE_NEW_PROCESS_GROUP,
		AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(parent)},
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}

func RunHelper(args []string, out, diagnostics io.Writer) (int, bool) {
	if len(args) == 0 || args[0] != helperArgument {
		return 0, false
	}
	if len(args) != 5 {
		return entry.ExitUsage, true
	}
	handle, err := strconv.ParseUint(args[1], 10, strconv.IntSize)
	stageName, tag := args[4], args[3]
	if err != nil || handle == 0 || !validRelease(tag) ||
		filepath.Base(stageName) != stageName || !strings.HasPrefix(stageName, stagePrefix) {
		return entry.ExitUsage, true
	}
	exe, err := os.Executable()
	if err != nil {
		return entry.ExitFailure, true
	}
	tx := transaction{backup: filepath.Dir(exe)}
	if !strings.HasPrefix(filepath.Base(tx.backup), backupPrefix) || filepath.Base(exe) != binaryNames[1] {
		return entry.ExitUsage, true
	}
	tx.dir = filepath.Dir(tx.backup)
	tx.stage = filepath.Join(tx.dir, stageName)
	if _, err := os.Stat(filepath.Join(tx.dir, corestart.InstallLockName)); err != nil {
		return entry.ExitFailure, true
	}
	if info, err := os.Lstat(tx.stage); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return entry.ExitFailure, true
	}
	ctx := context.Background()
	console := commandIO{tr: i18n.New(i18n.Language(args[2])), out: out, diagnostics: diagnostics}
	parent := windows.Handle(handle)
	exited, err := waitHandle(parent, 30_000)
	_ = windows.CloseHandle(parent)
	if err != nil || !exited {
		tx.cleanup()
		if err == nil {
			err = errors.New("the previous CLI is still running")
		}
		return console.fail(ctx, issue("upgrade.install_failed", err)), true
	}
	return console.finish(tx, tag), true
}

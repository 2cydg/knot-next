package upgrade

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"knot-cli/internal/corestart"
)

const backupPrefix = ".knot-backup."
const stagePrefix = ".knot-upgrade."

var errRestore = errors.New("rollback failed")

var executableSuffix = func() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}()

var binaryNames = [2]string{"knot-core" + executableSuffix, "knot" + executableSuffix}

type installation struct {
	dir   string
	files [2]os.FileInfo
}

func localInstallation() (installation, error) {
	exe, err := os.Executable()
	if err != nil {
		return installation{}, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return installation{}, err
	}
	if filepath.Base(exe) != binaryNames[1] {
		return installation{}, fmt.Errorf("expected %s", binaryNames[1])
	}
	return inspectInstallation(filepath.Dir(exe))
}

func inspectInstallation(dir string) (installation, error) {
	pair := installation{dir: dir}
	for i, name := range binaryNames {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return installation{}, err
		}
		if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0 {
			return installation{}, fmt.Errorf("expected a regular executable at %s", info.Name())
		}
		pair.files[i] = info
	}
	return pair, nil
}

func (p installation) checkUnchanged() error {
	current, err := inspectInstallation(p.dir)
	if err != nil {
		return err
	}
	for i, file := range p.files {
		now := current.files[i]
		if !os.SameFile(file, now) || file.Size() != now.Size() || !file.ModTime().Equal(now.ModTime()) {
			return fmt.Errorf("%s changed during the upgrade", binaryNames[i])
		}
	}
	return nil
}

func binaryVersion(ctx context.Context, path, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 || fields[0] != name || !versionPattern.MatchString(fields[1]) {
		return "", errors.New("unrecognized version output")
	}
	return fields[1], nil
}

func (p installation) coreVersion(ctx context.Context, s source, cliVersion string) (string, error) {
	version, err := binaryVersion(ctx, filepath.Join(p.dir, binaryNames[0]), "knot-core")
	if err == nil {
		return version, nil
	}
	// Older Core releases had no --version. Match the official asset by
	// checksum before using the paired CLI's tag as its installed version.
	tag := "v" + strings.TrimPrefix(cliVersion, "v")
	if !versionPattern.MatchString(tag) || developmentBuild(tag) {
		return "", err
	}
	checksums, err := s.checksums(ctx, tag)
	if err != nil {
		return "", err
	}
	file, err := os.Open(filepath.Join(p.dir, binaryNames[0]))
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, binaryLimit+1))
	if err != nil {
		return "", err
	}
	if size == 0 || size > binaryLimit || [sha256.Size]byte(hash.Sum(nil)) != checksums[assetName("core")] {
		return "", errors.New("could not identify the installed Core version")
	}
	return cliVersion, nil
}

func comparePair(cliVersion, coreVersion, tag string) (older, newer bool, err error) {
	cli, err := compareStable(cliVersion, tag)
	if err != nil {
		return false, false, err
	}
	core, err := compareStable(coreVersion, tag)
	if err != nil {
		return false, false, err
	}
	return cli < 0 || core < 0, cli > 0 || core > 0, nil
}

type transaction struct {
	dir, stage, backup string
}

func (p installation) prepare() (transaction, error) {
	lock := filepath.Join(p.dir, corestart.InstallLockName)
	file, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return transaction{}, err
	}
	err = file.Close()
	if err == nil {
		var stage string
		stage, err = os.MkdirTemp(p.dir, stagePrefix)
		if err == nil {
			return transaction{dir: p.dir, stage: stage}, nil
		}
	}
	_ = os.Remove(lock)
	return transaction{}, err
}

func (tx transaction) cleanup() {
	_ = os.RemoveAll(tx.stage)
	_ = os.Remove(filepath.Join(tx.dir, corestart.InstallLockName))
}

func (tx *transaction) backUp() error {
	dir, err := os.MkdirTemp(tx.dir, backupPrefix)
	if err != nil {
		return err
	}
	for _, name := range binaryNames {
		if err := copyBinary(filepath.Join(tx.dir, name), filepath.Join(dir, name)); err != nil {
			_ = os.RemoveAll(dir)
			return err
		}
	}
	tx.backup = dir
	return nil
}

func copyBinary(src, dst string) error {
	input, err := os.Open(src)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	output, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(output, input)
	if err != nil {
		output.Close()
		return err
	}
	return errors.Join(output.Chmod(info.Mode().Perm()), output.Sync(), output.Close())
}

func (tx transaction) replace() error {
	core, cli := binaryNames[0], binaryNames[1]
	if err := os.Rename(filepath.Join(tx.stage, core), filepath.Join(tx.dir, core)); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(tx.stage, cli), filepath.Join(tx.dir, cli)); err != nil {
		// Restore Core if the CLI could not be changed. Keep the original
		// backup until the restore has succeeded.
		restore := filepath.Join(tx.stage, "restore-"+core)
		if restoreErr := copyBinary(filepath.Join(tx.backup, core), restore); restoreErr != nil {
			return errors.Join(err, errRestore, restoreErr)
		}
		if restoreErr := os.Rename(restore, filepath.Join(tx.dir, core)); restoreErr != nil {
			return errors.Join(err, errRestore, restoreErr)
		}
		return err
	}
	return nil
}

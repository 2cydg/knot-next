// Package upgrade manages the official, same-directory Knot CLI and Core pair.
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"knot-cli/internal/buildinfo"
	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/preferences"
	"knot-cli/internal/terminal"
)

func Run(ctx context.Context, tr i18n.Translator, input *os.File, out, diagnostics io.Writer) int {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	console := commandIO{tr: tr, out: out, diagnostics: diagnostics}
	if developmentBuild(buildinfo.Version) || buildinfo.Dirty == "true" {
		return console.fail(ctx, issue("upgrade.dev", nil))
	}
	pair, err := localInstallation()
	if err != nil {
		return console.fail(ctx, issue("upgrade.installation", err))
	}
	cliVersion, err := binaryVersion(ctx, filepath.Join(pair.dir, binaryNames[1]), "knot")
	if err != nil || cliVersion != buildinfo.Version {
		return console.fail(ctx, issue("upgrade.installation", errors.Join(errors.New("CLI on disk does not match the running version"), err)))
	}
	confirm := func(ctx context.Context) (bool, error) {
		if input == nil || !term.IsTerminal(int(input.Fd())) {
			return false, errConfirmationTerminal
		}
		line, err := terminal.ReadLine(ctx, input)
		if err != nil {
			return false, err
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "y" || answer == "yes", nil
	}
	return console.run(ctx, pair, cliVersion, officialSource(), confirm)
}

func Available(ctx context.Context) string {
	if developmentBuild(buildinfo.Version) || buildinfo.Dirty == "true" {
		return ""
	}
	pair, err := localInstallation()
	if err != nil {
		return ""
	}
	source := officialSource()
	tag, err := source.cachedLatest(ctx)
	if err != nil {
		return ""
	}
	core, err := pair.coreVersion(ctx, source, buildinfo.Version)
	if err != nil || developmentBuild(core) {
		return ""
	}
	older, newer, _ := comparePair(buildinfo.Version, core, tag)
	if older && !newer {
		return tag
	}
	return ""
}

var errConfirmationTerminal = errors.New("confirmation requires an interactive terminal")

type commandIO struct {
	tr               i18n.Translator
	out, diagnostics io.Writer
}

type failure struct {
	key i18n.Key
	err error
}

func issue(key i18n.Key, err error) *failure { return &failure{key: key, err: err} }

func (c commandIO) fail(ctx context.Context, failure *failure) int {
	if ctx.Err() == context.Canceled {
		fmt.Fprintln(c.diagnostics, c.tr.Message("upgrade.canceled"))
		return entry.ExitCanceled
	}
	var params map[string]any
	key := failure.key
	if errors.Is(failure.err, errConfirmationTerminal) {
		key = "upgrade.confirm_terminal"
	} else if errors.Is(failure.err, errOtherCore) {
		key = "upgrade.core_other"
	} else if failure.err != nil {
		reason := failure.err.Error()
		var protocol *coreclient.Error
		if errors.As(failure.err, &protocol) {
			reason = c.tr.ProtocolError(protocol.Code)
		}
		params = map[string]any{"Reason": reason}
	}
	fmt.Fprintln(c.diagnostics, c.tr.Text(key, params))
	return entry.ExitFailure
}

func (c commandIO) run(ctx context.Context, pair installation, cliVersion string, source source, confirm func(context.Context) (bool, error)) int {
	tag, err := source.latest(ctx)
	if err != nil {
		return c.fail(ctx, issue("upgrade.check_failed", err))
	}
	coreVersion, err := pair.coreVersion(ctx, source, cliVersion)
	if err != nil {
		return c.fail(ctx, issue("upgrade.installation", err))
	}
	if developmentBuild(coreVersion) {
		return c.fail(ctx, issue("upgrade.dev", nil))
	}
	older, newer, err := comparePair(cliVersion, coreVersion, tag)
	if err != nil {
		return c.fail(ctx, issue("upgrade.installation", err))
	}
	params := map[string]any{"Tag": tag}
	if older && newer {
		return c.fail(ctx, issue("upgrade.downgrade", nil))
	}
	if !older {
		if newer {
			return c.result("upgrade.newer", params)
		}
		return c.result("upgrade.current", nil)
	}

	settings, err := preferences.Load()
	if err != nil {
		return c.fail(ctx, issue("upgrade.core_inspect", err))
	}
	config := preferences.Core{URL: preferences.DefaultCoreURL}
	if settings.Core != nil && strings.TrimSpace(settings.Core.URL) != "" {
		config = *settings.Core
	}
	client, err := coreclient.New(config.URL, config.Secret)
	if err != nil {
		return c.fail(ctx, issue("upgrade.core_inspect", err))
	}
	defer client.Close()
	info, err := probeCore(ctx, client)
	if err != nil {
		return c.fail(ctx, issue("upgrade.core_inspect", err))
	}
	running, err := inspectCore(info, filepath.Join(pair.dir, binaryNames[0]))
	if err != nil {
		return c.fail(ctx, issue("upgrade.core_inspect", err))
	}
	prompt := i18n.Key("upgrade.prompt")
	if running != nil {
		defer running.watch.close()
		prompt = "upgrade.core_prompt"
	}
	if _, err := fmt.Fprint(c.out, c.tr.Text(prompt, params)); err != nil {
		return c.fail(ctx, issue(i18n.OutputFailed, err))
	}
	yes, err := confirm(ctx)
	if errors.Is(err, io.EOF) {
		yes, err = false, nil
	}
	if err != nil {
		return c.fail(ctx, issue("upgrade.confirm_failed", err))
	}
	if !yes {
		return c.result("upgrade.canceled", nil)
	}
	code, problem := c.apply(ctx, pair, source, tag, client, running)
	if problem != nil {
		return c.fail(ctx, problem)
	}
	return code
}

func (c commandIO) result(key i18n.Key, params map[string]any) int {
	if _, err := fmt.Fprintln(c.out, c.tr.Text(key, params)); err != nil {
		return c.fail(context.Background(), issue(i18n.OutputFailed, err))
	}
	return entry.ExitOK
}

func (c commandIO) apply(ctx context.Context, pair installation, source source, tag string, client *coreclient.Client, running *runningCore) (int, *failure) {
	tx, err := pair.prepare()
	if err != nil {
		return 0, issue("upgrade.locked", err)
	}
	ownsStage := true
	defer func() {
		if ownsStage {
			tx.cleanup()
		}
	}()
	if err := pair.checkUnchanged(); err != nil {
		return 0, issue("upgrade.installation", err)
	}
	fmt.Fprintln(c.out, c.tr.Text("upgrade.downloading", map[string]any{"Tag": tag}))
	checksums, err := source.checksums(ctx, tag)
	if err != nil {
		return 0, issue("upgrade.download_failed", err)
	}
	for i, component := range []string{"core", "cli"} {
		asset := assetName(component)
		if err := source.download(ctx, tag, asset, filepath.Join(tx.stage, binaryNames[i]), checksums[asset], pair.files[i].Mode().Perm()); err != nil {
			return 0, issue("upgrade.download_failed", err)
		}
	}
	if err := pair.checkUnchanged(); err != nil {
		return 0, issue("upgrade.installation", err)
	}
	if err := tx.backUp(); err != nil {
		return 0, issue("upgrade.install_failed", err)
	}
	if problem := c.stopCore(ctx, client, running); problem != nil {
		return 0, problem
	}
	if ctx.Err() != nil {
		return 0, issue("upgrade.canceled", ctx.Err())
	}
	if err := pair.checkUnchanged(); err != nil {
		return 0, issue("upgrade.installation", err)
	}
	ownsStage = false
	return c.installPrepared(tx, tag), nil
}

// installPrepared owns cleanup unless it successfully hands the transaction
// to the Windows helper.
func (c commandIO) installPrepared(tx transaction, tag string) int {
	if runtime.GOOS == "windows" {
		if err := startHelper(tx, tag, c); err != nil {
			tx.cleanup()
			return c.fail(context.Background(), issue("upgrade.install_failed", err))
		}
		return c.result("upgrade.handoff", nil)
	}
	return c.finish(tx, tag)
}

func (c commandIO) stopCore(ctx context.Context, client *coreclient.Client, running *runningCore) *failure {
	current, err := probeCore(ctx, client)
	if err != nil {
		return issue("upgrade.core_inspect", err)
	}
	if current.InstanceID != "" &&
		(running == nil || current.InstanceID != running.runtime.InstanceID || current.PID != running.runtime.PID) {
		return issue("upgrade.core_changed", nil)
	}
	if running == nil {
		return nil
	}
	fmt.Fprintln(c.out, c.tr.Message("upgrade.core_stopping"))
	stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if current.InstanceID != "" {
		if err := client.Shutdown(stopCtx); err != nil && coreclient.Code(err) != "UNREACHABLE" {
			return issue("upgrade.core_stop_failed", err)
		}
	}
	if err := running.wait(stopCtx); err != nil {
		return issue("upgrade.core_stop_failed", err)
	}
	current, err = probeCore(stopCtx, client)
	if err != nil {
		return issue("upgrade.core_stop_failed", err)
	}
	if current.InstanceID != "" {
		return issue("upgrade.core_changed", nil)
	}
	return nil
}

func (c commandIO) finish(tx transaction, tag string) int {
	err := tx.replace()
	if errors.Is(err, errRestore) {
		// Keep the installation locked so auto-start cannot launch a mixed pair.
		fmt.Fprintln(c.diagnostics, c.tr.Text("upgrade.restore_failed", map[string]any{"Path": tx.backup}))
	} else {
		tx.cleanup()
	}
	if err != nil {
		return c.fail(context.Background(), issue("upgrade.install_failed", err))
	}
	return c.result("upgrade.success", map[string]any{"Tag": tag, "Path": tx.backup})
}

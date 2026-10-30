package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	stdhttp "net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	apihttp "knot-core/internal/api/http"
	"knot-core/internal/auth"
	"knot-core/internal/lifecycle"
	"knot-core/internal/logger"
	"knot-core/internal/paths"
	coreruntime "knot-core/internal/runtime"
	"knot-core/pkg/config"
	"knot-core/pkg/core"
	"knot-core/pkg/crypto"
	"knot-core/pkg/forward"
	"knot-core/pkg/secret"
	"knot-core/pkg/session"
	"knot-core/pkg/sftp"
	"knot-core/pkg/sshpool"
)

const (
	defaultPort = config.DefaultServicePort

	// shutdownGracePeriod bounds the graceful phase of teardown. It lives here
	// rather than at the call site so every trigger — the API, a signal, or a
	// serve error — is bounded by the same value.
	shutdownGracePeriod = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Fatal(logger.Redact(err.Error()))
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "init" {
		if len(os.Args) != 2 {
			return fmt.Errorf("usage: knot-core init")
		}
		return initializeConfig("config.toml", os.Stdout)
	}
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: knot-core [--config <file>]\n       knot-core init (create config.toml without starting core)")
		flag.PrintDefaults()
	}

	var configFile string
	flag.StringVar(&configFile, "config", "", "core TOML configuration file (defaults to the user configuration)")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments; use knot-core --help")
	}

	startedAt := time.Now().UTC()

	// Only argument parsing and path computation happen before the instance
	// lock: neither reads nor writes shared state, so a second instance is
	// rejected before it can touch the token, crypto provider, or pool.
	layout, err := paths.DefaultLayout()
	if err != nil {
		return fmt.Errorf("resolve paths: %w", err)
	}

	if configFile != "" {
		layout, err = paths.WithConfig(configFile)
		if err != nil {
			return fmt.Errorf("resolve configuration: %w", err)
		}
	}
	startup, err := config.LoadService(layout, configFile != "")
	if err != nil {
		return err
	}
	runtimeInfo := coreruntime.NewHolder()
	connTracker := apihttp.NewConnTracker()

	runner, err := lifecycle.New(lifecycle.Config{
		Layout:      layout,
		Port:        startup.Port,
		Version:     core.DefaultVersion,
		APIVersion:  core.APIVersion,
		StartedAt:   startedAt,
		GracePeriod: shutdownGracePeriod,
		Runtime:     runtimeInfo,
		Server: lifecycle.ServerConfig{
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      0,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    1 << 20,
		},
		Prepare: func(ctx context.Context, env lifecycle.Env) (stdhttp.Handler, error) {
			return prepareServices(env, layout, startedAt, startup, runtimeInfo, connTracker)
		},
	})
	if err != nil {
		return fmt.Errorf("create runner: %w", err)
	}

	// Setup signal handling
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()

	// Start the service. Shutdown of the shared resources happens inside the
	// runner, so this path and the API path cannot diverge.
	if err := runner.Start(ctx); err != nil {
		return fmt.Errorf("start service: %w", err)
	}

	logger.Diagnostic(slog.LevelInfo, "knot-core started successfully")

	// Wait for the single teardown flow to finish. It is triggered by the
	// authenticated shutdown API, by SIGINT/SIGTERM, or by a serve error, and it
	// bounds itself, so this cannot wait forever on a stuck handler.
	runner.Wait()

	if err := runner.Shutdown(context.Background()); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	return nil
}

// prepareServices builds every service that touches shared state. It runs while
// the lifecycle runner holds the single-instance lock, so a second instance can
// never reach this code.
func prepareServices(
	env lifecycle.Env,
	layout paths.Layout,
	startedAt time.Time,
	startup config.ServiceSettings,
	runtimeInfo *coreruntime.Holder,
	connTracker *apihttp.ConnTracker,
) (handler stdhttp.Handler, resultErr error) {
	defer func() {
		if resultErr != nil {
			logger.Diagnostic(slog.LevelError, "core.prepare_failed", "error", logger.Redact(resultErr.Error()))
		}
	}()
	file, err := logger.Open(layout.LogPath, logger.Options{})
	if err != nil {
		return nil, fmt.Errorf("initialize file logger: %w", err)
	}
	previous := slog.Default()
	previousWriter, previousFlags := log.Writer(), log.Flags()
	previousRedactor := logger.DefaultRedactor()
	logger.UseRedactor(file.Redactor())
	slog.SetDefault(file.Logger().With("instance_id", env.InstanceID))
	logger.Diagnostic(slog.LevelInfo, "core.starting")
	env.OnCleanup(func(context.Context) error {
		logger.Diagnostic(slog.LevelInfo, "core.stopped")
		err := file.Close()
		slog.SetDefault(previous)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		logger.UseRedactor(previousRedactor)
		return err
	})
	token := startup.Secret
	env.SetTokenPresent(token != "")
	file.Redactor().Add(token)

	if err := config.RecoverBootstrap(layout); err != nil {
		return nil, fmt.Errorf("recover crypto bootstrap: %w", err)
	}
	cryptoProvider, err := crypto.NewDefaultProvider(layout)
	if err != nil {
		return nil, fmt.Errorf("initialize crypto provider: %w", err)
	}

	configService := config.NewService(layout, cryptoProvider)
	if err := configService.Initialize(); err != nil {
		return nil, fmt.Errorf("open configuration: %w", err)
	}
	settings, err := configService.RuntimeConfig()
	if err != nil {
		return nil, fmt.Errorf("load runtime configuration: %w", err)
	}
	var level slog.Level
	if err = level.UnmarshalText([]byte(settings.Settings.LogLevel)); err == nil {
		// The handler observes level changes through its shared LevelVar.
		file.SetLevel(level)
	}
	secretService := secret.NewService(configService, cryptoProvider)
	sharedPool := sshpool.NewPool()
	sessionService := session.NewService()
	sessionService.UseConfig(configService)
	sessionService.UsePool(sharedPool)
	sessionService.UseContext(env.Context)
	sftpService := sftp.NewService(filepath.Join(layout.StateDir, "sftp"))
	sftpService.UseConfig(configService)
	sftpService.UseSession(sessionService)
	sftpService.UsePool(sharedPool)
	sessionService.StartMaintenance(env.Context)
	sftpService.StartMaintenance(env.Context)

	coreService := core.New(core.DefaultVersion, startedAt)
	coreService.UseLogger(file)
	coreService.UseConfig(configService)
	coreService.UseSecret(secretService)
	coreService.UseSession(sessionService)
	coreService.UseSFTP(sftpService)
	coreService.UseSSHPool(sharedPool)
	forwardService := forward.NewService(sharedPool)
	sharedPool.UseTargetHook(forwardService.EnsureAutoForwards)
	coreService.UseForward(forwardService)
	// The shutdown API only asks for shutdown; the runner owns the teardown and
	// releases resources through the cleanups below.
	coreService.UseShutdown(env.RequestShutdown)

	// Cleanups run in reverse registration order: WebSocket connections are
	// closed before the services they are attached to are torn down.
	env.OnCleanup(func(ctx context.Context) error {
		return coreService.Shutdown(ctx)
	})
	env.OnCleanup(func(context.Context) error {
		connTracker.CloseAll()
		return nil
	})

	logger.Diagnostic(slog.LevelInfo, "core.services_ready")
	return apihttp.NewServer(
		coreService,
		runtimeInfo,
		auth.NewVerifier(token),
		auth.NewOriginChecker(startup.AllowedOrigins),
		apihttp.WithConnTracker(connTracker),
	).Handler(), nil
}

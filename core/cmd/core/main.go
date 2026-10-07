package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	stdhttp "net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	apihttp "knot-core/internal/api/http"
	"knot-core/internal/auth"
	"knot-core/internal/lifecycle"
	"knot-core/internal/paths"
	coreruntime "knot-core/internal/runtime"
	"knot-core/pkg/config"
	"knot-core/pkg/core"
	"knot-core/pkg/crypto"
	"knot-core/pkg/secret"
	"knot-core/pkg/session"
	"knot-core/pkg/sftp"
	"knot-core/pkg/sshpool"
)

const (
	defaultPort = 17898

	// shutdownGracePeriod bounds the graceful phase of teardown. It lives here
	// rather than at the call site so every trigger — the API, a signal, or a
	// serve error — is bounded by the same value.
	shutdownGracePeriod = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var port int
	var origins string
	flag.IntVar(&port, "port", envInt("KNOT_CORE_PORT", defaultPort), "loopback HTTP port")
	flag.StringVar(&origins, "allowed-origin", "", "comma-separated browser origins allowed to call the local API")
	flag.Parse()

	startedAt := time.Now().UTC()

	// Only argument parsing and path computation happen before the instance
	// lock: neither reads nor writes shared state, so a second instance is
	// rejected before it can touch the token, crypto provider, or pool.
	layout, err := paths.DefaultLayout()
	if err != nil {
		return fmt.Errorf("resolve paths: %w", err)
	}

	runtimeInfo := coreruntime.NewHolder()
	connTracker := apihttp.NewConnTracker()

	runner, err := lifecycle.New(lifecycle.Config{
		Layout:      layout,
		Port:        port,
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
			return prepareServices(env, layout, startedAt, origins, runtimeInfo, connTracker)
		},
	})
	if err != nil {
		return fmt.Errorf("create runner: %w", err)
	}

	// Setup signal handling
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start the service. Shutdown of the shared resources happens inside the
	// runner, so this path and the API path cannot diverge.
	if err := runner.Start(ctx); err != nil {
		return fmt.Errorf("start service: %w", err)
	}

	log.Printf("knot-core started successfully")

	// Wait for the single teardown flow to finish. It is triggered by the
	// authenticated shutdown API, by SIGINT/SIGTERM, or by a serve error, and it
	// bounds itself, so this cannot wait forever on a stuck handler.
	runner.Wait()

	if err := runner.Shutdown(context.Background()); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	log.Printf("knot-core stopped")
	return nil
}

// prepareServices builds every service that touches shared state. It runs while
// the lifecycle runner holds the single-instance lock, so a second instance can
// never reach this code.
func prepareServices(
	env lifecycle.Env,
	layout paths.Layout,
	startedAt time.Time,
	origins string,
	runtimeInfo *coreruntime.Holder,
	connTracker *apihttp.ConnTracker,
) (stdhttp.Handler, error) {
	token, err := auth.TokenStore{Path: layout.TokenPath}.LoadOrCreate()
	if err != nil {
		return nil, fmt.Errorf("load token: %w", err)
	}
	// Discovery reports the token that actually exists, not a placeholder.
	env.SetTokenPresent(token != "")

	cryptoProvider, err := crypto.NewDefaultProvider(layout)
	if err != nil {
		return nil, fmt.Errorf("initialize crypto provider: %w", err)
	}

	configService := config.NewService(layout, cryptoProvider)
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

	coreService := core.New(core.DefaultVersion, startedAt)
	coreService.UseConfig(configService)
	coreService.UseSecret(secretService)
	coreService.UseSession(sessionService)
	coreService.UseSFTP(sftpService)
	coreService.UseSSHPool(sharedPool)
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

	return apihttp.NewServer(
		coreService,
		runtimeInfo,
		auth.NewVerifier(token),
		auth.NewOriginChecker(splitCSV(origins)),
		apihttp.WithConnTracker(connTracker),
	).Handler(), nil
}

func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

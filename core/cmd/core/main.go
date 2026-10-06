package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	stdhttp "net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	apihttp "knot-core/internal/api/http"
	"knot-core/internal/auth"
	"knot-core/internal/paths"
	coreruntime "knot-core/internal/runtime"
	"knot-core/internal/transport"
	"knot-core/pkg/config"
	"knot-core/pkg/core"
	"knot-core/pkg/crypto"
	"knot-core/pkg/secret"
	"knot-core/pkg/session"
	"knot-core/pkg/sftp"
	"knot-core/pkg/sshpool"
)

const defaultPort = 17898

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
	layout, err := paths.DefaultLayout()
	if err != nil {
		return fmt.Errorf("resolve paths: %w", err)
	}
	if err := layout.Ensure(); err != nil {
		return fmt.Errorf("prepare paths: %w", err)
	}

	token, err := auth.TokenStore{Path: layout.TokenPath}.LoadOrCreate()
	if err != nil {
		return fmt.Errorf("load token: %w", err)
	}

	listeners, addresses, err := transport.ListenLoopback(port)
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}
	serverOwnsListeners := false
	defer func() {
		if !serverOwnsListeners {
			closeListeners(listeners)
		}
	}()
	actualPort := listeners[0].Addr().(*net.TCPAddr).Port

	runtimeInfo := coreruntime.NewInfo(core.DefaultVersion, core.APIVersion, actualPort, addresses, layout, startedAt, token != "")
	if err := coreruntime.WriteInfo(layout.RuntimePath, runtimeInfo); err != nil {
		return fmt.Errorf("write runtime info: %w", err)
	}

	cryptoProvider, err := crypto.NewDefaultProvider(layout)
	if err != nil {
		return fmt.Errorf("initialize crypto provider: %w", err)
	}
	configService := config.NewService(layout, cryptoProvider)
	secretService := secret.NewService(configService, cryptoProvider)
	sharedPool := sshpool.NewPool()
	sessionService := session.NewService()
	sessionService.UseConfig(configService)
	sessionService.UsePool(sharedPool)
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
	shutdownRequested := make(chan struct{})
	var requestShutdown sync.Once
	coreService.UseShutdown(func() {
		requestShutdown.Do(func() {
			_ = os.Remove(layout.RuntimePath)
			close(shutdownRequested)
		})
	})
	server := &stdhttp.Server{
		Handler:           apihttp.NewServer(coreService, runtimeInfo, auth.NewVerifier(token), auth.NewOriginChecker(splitCSV(origins))).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	errCh := make(chan error, len(listeners))
	var wg sync.WaitGroup
	serverOwnsListeners = true
	for _, ln := range listeners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.Serve(ln); err != nil && !errors.Is(err, stdhttp.ErrServerClosed) {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}

	log.Printf("knot-core listening on %s", strings.Join(addresses, ", "))
	select {
	case <-ctx.Done():
	case <-shutdownRequested:
	case err := <-errCh:
		_ = server.Close()
		wg.Wait()
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	wg.Wait()
	_ = os.Remove(layout.RuntimePath)
	return nil
}

func closeListeners(listeners []net.Listener) {
	for _, ln := range listeners {
		_ = ln.Close()
	}
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

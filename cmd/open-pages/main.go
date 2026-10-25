package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gira0/open-pages/internal/cli"
	"github.com/gira0/open-pages/internal/config"
	"github.com/gira0/open-pages/internal/logging"
	"github.com/gira0/open-pages/internal/server"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "deploy" {
		os.Exit(cli.Deploy(os.Args[2:]))
	}
	configPath := flag.String("config", "settings.ini", "path to the settings file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		// Startup errors can carry text from files or the network (for example SAML metadata);
		// keep them on one log line.
		msg := strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\r", " "), "\n", " ")
		slog.Error("fatal", "err", msg)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	slog.SetDefault(logging.New(os.Stderr, cfg.LogLevel, cfg.LogFormat))
	srv, err := server.New(cfg)
	if err != nil {
		return err
	}
	defer srv.Close()
	if !cfg.CookieSecure {
		slog.Warn("cookie_secure is false: the session cookie is sent without the Secure flag and without the __Host- prefix, so pages on sibling hosts can overwrite it; set cookie_secure = true when serving over HTTPS")
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 2)
	if ms := srv.MetricsServer(); ms != nil {
		defer ms.Close()
		go func() {
			slog.Info("metrics listening", "addr", ms.Addr)
			if err := ms.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				serveErr <- err
			}
		}()
	}
	go func() {
		slog.Info("listening", "addr", cfg.Listen, "data", srv.SitesDir())
		serveErr <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	// Stop accepting connections and wait for in-flight requests before the
	// deferred database close runs.
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

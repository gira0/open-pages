package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
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
	configPath := flag.String("config", defaultConfig, "path to the settings file")
	flag.Parse()

	// The default path may be absent when OPEN_PAGES_* variables supply every setting; a
	// path given with -config must exist.
	explicit := false
	flag.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "config" })
	if explicit && *configPath == "" {
		// An empty -config (say from an unset variable) must not turn into the env-only mode.
		slog.Error("fatal", "err", "-config was given an empty path")
		os.Exit(1)
	}
	noFile := false
	if _, err := os.Stat(*configPath); errors.Is(err, fs.ErrNotExist) && !explicit {
		noFile = true
		*configPath = ""
	}

	if err := run(*configPath, noFile); err != nil {
		// Startup errors can carry text from files or the network (for example SAML metadata);
		// keep them on one log line.
		msg := strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\r", " "), "\n", " ")
		slog.Error("fatal", "err", msg)
		os.Exit(1)
	}
}

// defaultConfig is the settings file read when -config is not given.
const defaultConfig = "settings.ini"

// run serves until a signal arrives. An empty configPath means there is no settings file
// and the environment supplies the settings; noFile only adds a log line saying so.
func run(configPath string, noFile bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	slog.SetDefault(logging.New(os.Stderr, cfg.LogLevel, cfg.LogFormat))
	if noFile {
		slog.Info("no settings file found, using OPEN_PAGES_* environment variables and defaults", "path", defaultConfig)
	}
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
		IdleTimeout:       120 * time.Second,
		// There is no WriteTimeout on purpose: it bounds the whole response, so it would cut
		// off large uploads' replies and slow downloads of big sites. Slow clients are
		// limited by the read header timeout, the idle timeout and the upload and deploy limits.
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

	// Fail /readyz first and give the load balancer drain_delay to stop sending requests,
	// then stop accepting connections and wait for in-flight requests before the
	// deferred database close runs.
	slog.Info("shutting down", "drain_delay", cfg.DrainDelay, "shutdown_timeout", cfg.ShutdownTimeout)
	srv.Drain()
	if cfg.DrainDelay > 0 {
		select {
		case <-time.After(cfg.DrainDelay):
		case err := <-serveErr:
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

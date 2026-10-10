package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gira0/open-pages/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "deploy" {
		os.Exit(runDeploy(os.Args[2:]))
	}
	configPath := flag.String("config", "settings.ini", "path to the settings file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	slog.SetDefault(newLogger(os.Stderr, cfg))
	srv, err := newServer(cfg)
	if err != nil {
		return err
	}
	defer srv.db.Close()

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 2)
	if ms := srv.metricsServer(); ms != nil {
		defer ms.Close()
		go func() {
			slog.Info("metrics listening", "addr", ms.Addr)
			if err := ms.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				serveErr <- err
			}
		}()
	}
	go func() {
		slog.Info("listening", "addr", cfg.Listen, "data", srv.sites)
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

// newServer prepares the data directories and database described by cfg.
func newServer(cfg Config) (*Server, error) {
	s := &Server{
		cfg:   cfg,
		sites: filepath.Join(cfg.DataPath, "op_data"),
		tmp:   filepath.Join(cfg.TmpPath, "tmp"),

		metrics: newMetrics(),
	}
	if cfg.OIDC.Enabled {
		s.oidc = newOIDCClient(cfg.OIDC)
	}
	// Sites are world-readable so a reverse proxy can serve them; spooled uploads are private.
	if err := os.MkdirAll(s.sites, 0o755); err != nil { //nolint:gosec // G301: public site content
		return nil, fmt.Errorf("create %s: %w", s.sites, err)
	}
	if err := os.MkdirAll(s.tmp, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", s.tmp, err)
	}

	tmpl, err := template.ParseFS(web.Templates, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("load templates: %w", err)
	}
	s.tmpl = tmpl

	s.db, err = openDB(filepath.Join(cfg.DataPath, "data.db"))
	if err != nil {
		return nil, err
	}
	return s, nil
}

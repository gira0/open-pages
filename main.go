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
)

func main() {
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
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()

	slog.Info("listening", "addr", cfg.Listen, "data", srv.sites)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
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
	}
	for _, dir := range []string{s.sites, s.tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	tmpl, err := template.ParseGlob("templates/*.html")
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

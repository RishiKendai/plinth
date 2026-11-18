// Command api runs the Plinth HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"plinth/server/internal/app"
	"plinth/server/internal/platform/config"
	"plinth/server/internal/platform/database"
	"plinth/server/internal/platform/logging"
	"plinth/server/internal/platform/security"
	"plinth/server/internal/platform/storage"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	log := logging.New(os.Stdout, cfg.LogLevel, cfg.IsProduction())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	var store storage.Store
	if cfg.R2.Configured() {
		s3, err := storage.NewS3(cfg.R2.Endpoint, cfg.R2.Bucket, cfg.R2.AccessKeyID, cfg.R2.SecretAccessKey)
		if err != nil {
			return err
		}
		store = s3
	} else {
		log.Warn("R2 is not configured: attachments are disabled")
	}
	handler, _ := app.New(app.Deps{Config: cfg, DB: db, Log: log, Hasher: security.DefaultHasher(), Store: store})
	srv := &http.Server{
		Addr: cfg.HTTPAddr, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("api started", "addr", cfg.HTTPAddr, "env", cfg.Env, "version", version)

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
	return nil
}

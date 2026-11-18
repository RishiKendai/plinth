// Command worker runs background jobs. Phase 1 job: purge expired sessions.
// Jobs must be safe to retry (CLAUDE.md section 12).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"plinth/server/internal/auth"
	"plinth/server/internal/platform/config"
	"plinth/server/internal/platform/database"
	"plinth/server/internal/platform/logging"
	"plinth/server/internal/platform/security"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
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
	svc := auth.NewService(auth.NewStore(db), security.DefaultHasher(), auth.Config{Pepper: cfg.TokenPepper}, log)

	purge := func() {
		jctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		s, err := svc.Purge(jctx)
		if err != nil {
			log.Error("purge failed", "err", err.Error())
			return
		}
		log.Info("purge done", "sessions", s)
	}
	log.Info("worker started", "version", version)
	purge()
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("worker stopped")
			return nil
		case <-tick.C:
			purge()
		}
	}
}

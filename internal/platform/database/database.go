// Package database opens the PostgreSQL (Neon) connection pool.
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/platform/config"
)

func Connect(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	return Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBUsePooler)
}

// Open creates a pool. usePooler must be true for Neon's pgbouncer endpoint: it disables
// server-side prepared statements, which transaction-mode pooling cannot keep.
func Open(ctx context.Context, url string, maxConns int32, usePooler bool) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("invalid database url: %w", sanitize(err))
	}
	pc.MaxConns = maxConns
	pc.MaxConnLifetime = 30 * time.Minute
	pc.MaxConnIdleTime = 5 * time.Minute            // Neon suspends idle computes; do not hold stale connections
	pc.ConnConfig.ConnectTimeout = 15 * time.Second // allows for Neon cold start
	if usePooler {
		pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", sanitize(err))
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database unreachable: %w", err) // pgx connect errors never include the password
	}
	return pool, nil
}

// sanitize drops detail that could contain the connection string.
func sanitize(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%T", err)
}

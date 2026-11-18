// Package testdb gives integration tests a real PostgreSQL. It uses TEST_DATABASE_URL if set (for example a
// dedicated Neon branch), otherwise it starts an embedded PostgreSQL. Financial correctness is never tested
// against mocks (CLAUDE.md section 11.1).
package testdb

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	embedded "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/platform/database"
	"plinth/server/migrations"
)

// Start returns a migrated pool and a stop function. Call from TestMain.
func Start() (pool *pgxpool.Pool, stop func(), err error) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	stop = func() {}
	if url == "" {
		port, perr := freePort()
		if perr != nil {
			return nil, stop, perr
		}
		dir, derr := os.MkdirTemp("", "plinth-pg-")
		if derr != nil {
			return nil, stop, derr
		}
		pg := embedded.NewDatabase(embedded.DefaultConfig().
			Port(uint32(port)).
			DataPath(filepath.Join(dir, "data")).RuntimePath(filepath.Join(dir, "run")).
			Username("plinth").Password("plinth").Database("plinth").
			StartTimeout(90 * time.Second))
		if serr := pg.Start(); serr != nil {
			return nil, stop, fmt.Errorf("start embedded postgres: %w", serr)
		}
		stop = func() { _ = pg.Stop(); _ = os.RemoveAll(dir) }
		url = fmt.Sprintf("postgres://plinth:plinth@127.0.0.1:%d/plinth?sslmode=disable", port)
	}
	pool, err = database.Open(ctx, url, 8, true) // same exec mode as production (Neon pooler)
	if err != nil {
		stop()
		return nil, func() {}, err
	}
	prev := stop
	stop = func() { pool.Close(); prev() }
	if err = Reset(ctx, pool); err != nil {
		stop()
		return nil, func() {}, err
	}
	return pool, stop, nil
}

// Reset drops everything and re-applies all up-migrations.
func Reset(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		return err
	}
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		if !strings.HasSuffix(n, ".up.sql") {
			continue
		}
		b, err := migrations.FS.ReadFile(n)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("migration %s: %w", n, err)
		}
	}
	return nil
}

// Truncate clears all data between tests.
func Truncate(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `TRUNCATE users, audit_events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	// The cascade also emptied the built-in categories.
	if _, err := pool.Exec(context.Background(), `SELECT plinth_seed_categories()`); err != nil {
		t.Fatal(err)
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

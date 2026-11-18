// Package dbx holds small database helpers shared by modules.
package dbx

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Q is satisfied by *pgxpool.Pool and pgx.Tx, so a function can run inside or outside a transaction.
type Q interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// IsNoRows reports whether err is "no rows in result set".
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// PgCode returns the SQLSTATE of err, or "" if it is not a PostgreSQL error.
func PgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// ConstraintName returns the violated constraint or index name, or "".
func ConstraintName(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.ConstraintName
	}
	return ""
}

const (
	UniqueViolation     = "23505"
	ForeignKeyViolation = "23503"
	CheckViolation      = "23514"
)

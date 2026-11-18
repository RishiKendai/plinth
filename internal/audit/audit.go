// Package audit records security-relevant events (CLAUDE.md section 5.6). Metadata must not contain secrets or financial content.
package audit

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgconn"
)

// Execer is satisfied by *pgxpool.Pool and pgx.Tx, so events can be written inside the caller's transaction.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func Log(ctx context.Context, db Execer, userID, event string, meta map[string]any) error {
	if meta == nil {
		meta = map[string]any{}
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	// Sent as text with an explicit cast: in pgx's pooler-safe Exec mode a []byte would be sent as bytea.
	var uid any
	if userID != "" {
		uid = userID
	}
	_, err = db.Exec(ctx, `INSERT INTO audit_events (user_id, event, metadata) VALUES ($1, $2, $3::jsonb)`, uid, event, string(b))
	return err
}

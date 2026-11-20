package users

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ObjectStore is the part of object storage the purge needs.
type ObjectStore interface {
	Delete(ctx context.Context, key string) error
}

// Purger permanently erases accounts whose deletion grace period has ended (docs/account-deletion.md).
type Purger struct {
	db    *pgxpool.Pool
	store ObjectStore // nil when R2 is not configured: then no stored files exist to delete
	log   *slog.Logger
	now   func() time.Time
}

func NewPurger(db *pgxpool.Pool, store ObjectStore, log *slog.Logger) *Purger {
	return &Purger{db: db, store: store, log: log, now: time.Now}
}

// PurgeDue erases up to limit users whose request is older than grace. A user who cancelled (the request is cleared) is never
// touched. Each user is erased in one database transaction, so either everything of theirs is gone or nothing is.
func (p *Purger) PurgeDue(ctx context.Context, grace time.Duration, limit int) (int, error) {
	rows, err := p.db.Query(ctx, `SELECT id::text FROM users WHERE deletion_requested_at IS NOT NULL AND deletion_requested_at < $1 ORDER BY deletion_requested_at LIMIT $2`, p.now().Add(-grace), limit)
	if err != nil {
		return 0, err
	}
	var idList []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		idList = append(idList, id)
	}
	rows.Close()
	done := 0
	for _, id := range idList {
		if err := p.Purge(ctx, id); err != nil {
			p.log.Error("account purge failed", "user_id", id, "err", err.Error())
			continue
		}
		p.log.Info("account purged", "user_id", id)
		done++
	}
	return done, nil
}

// Purge erases one user and everything they own. Shared group records other people rely on are kept: the user becomes a plain named
// member (no account) and the user's own ledger transactions are removed from them. A group nobody else uses is deleted.
func (p *Purger) Purge(ctx context.Context, userID string) error {
	// Stored files first: deleting an object twice is harmless, so a failure here simply leaves the user for the next run.
	if p.store != nil {
		rows, err := p.db.Query(ctx, `SELECT object_key FROM attachments WHERE user_id=$1 AND NOT object_removed`, userID)
		if err != nil {
			return err
		}
		var keys []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		rows.Close()
		for _, k := range keys {
			if err := p.store.Delete(ctx, k); err != nil {
				return fmt.Errorf("delete stored file: %w", err)
			}
		}
	}
	return pgx.BeginFunc(ctx, p.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL plinth.account_purge = 'on'`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, userID); err != nil {
			return err
		}
		// 1. Groups. One nobody else uses is deleted; a shared one keeps its records with this user turned into a named member.
		rows, err := tx.Query(ctx, `SELECT g.id::text FROM groups g JOIN group_members m ON m.group_id=g.id AND m.user_id=$1`, userID)
		if err != nil {
			return err
		}
		var groupIDs []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			groupIDs = append(groupIDs, id)
		}
		rows.Close()
		for _, gid := range groupIDs {
			var others int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM group_members WHERE group_id=$1 AND user_id IS NOT NULL AND user_id <> $2 AND removed_at IS NULL`, gid, userID).Scan(&others); err != nil {
				return err
			}
			if others == 0 {
				for _, q := range []string{
					`DELETE FROM expense_shares WHERE expense_id IN (SELECT id FROM group_expenses WHERE group_id=$1)`,
					`DELETE FROM group_expenses WHERE group_id=$1`, `DELETE FROM group_settlements WHERE group_id=$1`, `DELETE FROM group_topups WHERE group_id=$1`,
					`DELETE FROM groups WHERE id=$1`,
				} {
					if _, err := tx.Exec(ctx, q, gid); err != nil {
						return err
					}
				}
				continue
			}
			var next string // another person on Plinth who takes over as creator, and as admin if no other admin is left
			if err := tx.QueryRow(ctx, `SELECT user_id::text FROM group_members WHERE group_id=$1 AND user_id IS NOT NULL AND user_id <> $2 AND removed_at IS NULL ORDER BY (role='admin') DESC, created_at LIMIT 1`, gid, userID).Scan(&next); err != nil {
				return err
			}
			type step struct {
				q    string
				args []any
			}
			for _, st := range []step{
				{`UPDATE groups SET created_by=$2 WHERE id=$1 AND created_by=$3`, []any{gid, next, userID}},
				{`DELETE FROM group_invites WHERE group_id=$1 AND created_by=$2`, []any{gid, userID}},
				{`UPDATE group_members SET role='admin' WHERE group_id=$1 AND user_id=$2 AND NOT EXISTS (SELECT 1 FROM group_members a WHERE a.group_id=$1 AND a.role='admin' AND a.removed_at IS NULL AND a.user_id IS DISTINCT FROM $3::uuid)`, []any{gid, next, userID}},
				{`UPDATE group_members SET user_id=NULL WHERE group_id=$1 AND user_id=$2`, []any{gid, userID}},
			} {
				if _, err := tx.Exec(ctx, st.q, st.args...); err != nil {
					return err
				}
			}
		}
		// The user's transactions are about to go; group records that pointed at them keep their amounts but lose the link.
		for _, t := range []string{"group_expenses", "group_settlements", "group_topups"} {
			if _, err := tx.Exec(ctx, `UPDATE `+t+` SET transaction_id=NULL WHERE transaction_id IN (SELECT id FROM transactions WHERE user_id=$1)`, userID); err != nil {
				return err
			}
		}
		// 2. Everything the user owns, children before parents.
		steps := []string{
			`DELETE FROM tracker_transactions WHERE tracker_id IN (SELECT id FROM trackers WHERE user_id=$1)`,
			`DELETE FROM trackers WHERE user_id=$1`,
			`DELETE FROM budget_limits WHERE budget_id IN (SELECT id FROM budgets WHERE user_id=$1)`,
			`DELETE FROM budgets WHERE user_id=$1`,
			`DELETE FROM import_rows WHERE batch_id IN (SELECT id FROM import_batches WHERE user_id=$1)`,
			`DELETE FROM import_batches WHERE user_id=$1`,
			`DELETE FROM sms_events WHERE user_id=$1`,
			`DELETE FROM upi_payments WHERE user_id=$1`,
			`DELETE FROM reports WHERE user_id=$1`,
			`DELETE FROM reminders WHERE user_id=$1`,
			`DELETE FROM recurring_occurrences WHERE user_id=$1`,
			`DELETE FROM recurring_schedules WHERE user_id=$1`,
			`DELETE FROM attachments WHERE user_id=$1`,
			`DELETE FROM ledger_entries WHERE user_id=$1`,
			`DELETE FROM transaction_tags WHERE transaction_id IN (SELECT id FROM transactions WHERE user_id=$1)`,
			`DELETE FROM transactions WHERE user_id=$1`,
			`DELETE FROM transfers WHERE user_id=$1`,
			`DELETE FROM tags WHERE user_id=$1`,
			`DELETE FROM accounts WHERE user_id=$1`,
			`DELETE FROM categories WHERE user_id=$1`,
			`DELETE FROM audit_events WHERE user_id=$1`,
			`DELETE FROM users WHERE id=$1`, // sessions, settings, idempotency keys and tokens go with it
		}
		for _, q := range steps {
			if _, err := tx.Exec(ctx, q, userID); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
		}
		return nil
	})
}

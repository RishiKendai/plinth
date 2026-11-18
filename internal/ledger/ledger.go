// Package ledger owns ledger_entries and the cached account balance (CLAUDE.md 3.5, 6.5).
//
// Rules (docs/balance-rules.md):
//   - An account's balance is opening_balance + SUM(ledger_entries.amount).
//   - Entries are append-only. Undoing a movement appends a reversing entry; nothing is edited or deleted.
//   - accounts.balance is a cache updated in the same database transaction as every entry, under a row lock,
//     and VerifyAccount proves it equals the derived value.
//   - Only this package writes ledger_entries or changes accounts.balance after creation.
package ledger

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
)

// Account is the slice of account state the ledger needs, read under a row lock.
type Account struct {
	ID       string
	Type     string
	Currency string
	Balance  int64
	Archived bool
}

var (
	ErrAccountNotFound = apierr.ErrNotFound
	ErrAccountArchived = apierr.New(http.StatusConflict, "account_archived", "This account is archived. Restore it before adding transactions.")
)

// LockAccount locks one account row owned by userID until the transaction ends. Every balance change must
// lock first so concurrent writes to the same account are serialised.
func LockAccount(ctx context.Context, tx pgx.Tx, userID, accountID string) (Account, error) {
	a := Account{ID: accountID}
	var archived *time.Time
	err := tx.QueryRow(ctx, `SELECT type, currency, balance, archived_at FROM accounts WHERE id=$1 AND user_id=$2 FOR UPDATE`,
		accountID, userID).Scan(&a.Type, &a.Currency, &a.Balance, &archived)
	if dbx.IsNoRows(err) {
		return a, ErrAccountNotFound
	}
	a.Archived = archived != nil
	return a, err
}

// LockAccounts locks several accounts in a fixed (sorted) order to avoid deadlocks between concurrent transfers.
func LockAccounts(ctx context.Context, tx pgx.Tx, userID string, accountIDs ...string) (map[string]Account, error) {
	sorted := append([]string(nil), accountIDs...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	out := map[string]Account{}
	for _, id := range sorted {
		if _, dup := out[id]; dup {
			continue
		}
		a, err := LockAccount(ctx, tx, userID, id)
		if err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, nil
}

// Post appends one movement for a transaction and updates the cached balance. The account must already be
// locked in tx. amount is signed minor units: negative leaves the account.
func Post(ctx context.Context, tx pgx.Tx, userID, accountID, transactionID, currency string, amount int64, effectiveAt time.Time, kind string) error {
	if amount == 0 {
		return fmt.Errorf("ledger: zero amount")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (user_id, account_id, transaction_id, amount, currency, kind, effective_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, userID, accountID, transactionID, amount, currency, kind, effectiveAt); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE accounts SET balance = balance + $2, updated_at = now() WHERE id=$1`, accountID, amount)
	return err
}

// Reverse appends a reversing entry that cancels whatever the transaction has posted so far. It is a no-op
// when the transaction has no net effect. The account must already be locked in tx.
func Reverse(ctx context.Context, tx pgx.Tx, userID, transactionID string) error {
	var accountID, currency string
	var net int64
	var effective time.Time
	err := tx.QueryRow(ctx, `SELECT account_id::text, currency, SUM(amount), max(effective_at) FROM ledger_entries
		WHERE transaction_id=$1 AND user_id=$2 GROUP BY account_id, currency`, transactionID, userID).Scan(&accountID, &currency, &net, &effective)
	if dbx.IsNoRows(err) || (err == nil && net == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	return Post(ctx, tx, userID, accountID, transactionID, currency, -net, effective, "reversal")
}

// VerifyAccount returns the cached balance and the balance derived from the ledger. They must be equal.
func VerifyAccount(ctx context.Context, q dbx.Q, accountID string) (cached, derived int64, err error) {
	err = q.QueryRow(ctx, `SELECT a.balance, a.opening_balance + COALESCE((SELECT SUM(amount) FROM ledger_entries WHERE account_id = a.id), 0)
		FROM accounts a WHERE a.id=$1`, accountID).Scan(&cached, &derived)
	return
}

// Counting reports whether a transaction in this status contributes to the account balance.
// Owner decision: pending and confirmed both count. Failed, cancelled, reversed and needs_review do not
// (a reversed transaction nets to zero through its reversing entry).
func Counting(status string) bool { return status == "pending" || status == "confirmed" }

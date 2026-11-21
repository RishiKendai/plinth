package app_test

import (
	"context"
	"testing"
	"time"

	"plinth/server/internal/idempotency"
)

// Keys are kept for their lifetime so a retry still replays, then deleted so the table does not grow without bound.
// Deleting a key never touches the money it protected.
func TestOldIdempotencyKeysArePurgedAndRecentOnesStillReplay(t *testing.T) {
	e := setup(t)
	ta, _, ua := e.signUp(t, "asha@example.com")
	acct := e.newAccount(t, ta, "HDFC", "savings", 1000000)
	oldKey, newK := newKey(), newKey()
	body := func(title string) map[string]any {
		return map[string]any{"account_id": acct, "type": "expense", "amount": 5000, "title": title}
	}
	first := e.call(t, "POST", "/v1/transactions", ta, newK, body("recent"))
	if first.Status != 201 {
		t.Fatalf("create: %d %s", first.Status, first.Raw)
	}
	if r := e.call(t, "POST", "/v1/transactions", ta, oldKey, body("old")); r.Status != 201 {
		t.Fatalf("create old: %d %s", r.Status, r.Raw)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE idempotency_keys SET created_at = now() - interval '31 days' WHERE user_id=$1 AND key=$2`, ua, oldKey); err != nil {
		t.Fatal(err)
	}
	txBefore := assertCount(t, `SELECT count(*) FROM transactions WHERE user_id='`+ua+`'`)
	ledgerBefore := assertCount(t, `SELECT count(*) FROM ledger_entries WHERE user_id='`+ua+`'`)

	n, err := idempotency.Purge(ctx, pool, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("purged %d, err %v; want exactly the old key", n, err)
	}
	if assertCount(t, `SELECT count(*) FROM idempotency_keys WHERE user_id='`+ua+`' AND key='`+oldKey+`'`) != 0 {
		t.Fatal("the old key should be gone")
	}
	// A recent key still protects against a duplicate: the retry replays the original and records nothing new.
	again := e.call(t, "POST", "/v1/transactions", ta, newK, body("recent"))
	if again.Status != 201 && again.Status != 200 {
		t.Fatalf("replay: %d %s", again.Status, again.Raw)
	}
	if assertCount(t, `SELECT count(*) FROM transactions WHERE user_id='`+ua+`'`) != txBefore {
		t.Fatal("a replay created another transaction")
	}
	// Purging never changes money.
	if assertCount(t, `SELECT count(*) FROM ledger_entries WHERE user_id='`+ua+`'`) != ledgerBefore {
		t.Fatal("purging keys changed the ledger")
	}
	if n2, _ := idempotency.Purge(ctx, pool, time.Now()); n2 != 0 {
		t.Fatalf("a second purge removed %d", n2)
	}
}

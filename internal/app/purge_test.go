package app_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"plinth/server/internal/users"
)

func purgerFor(e *env) *users.Purger {
	return users.NewPurger(pool, e.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func userCount(t *testing.T, id string) int {
	return assertCount(t, `SELECT count(*) FROM users WHERE id='`+id+`'`)
}

func TestAccountPurgeErasesEverythingAfterTheGracePeriodAndKeepsOtherPeoplesData(t *testing.T) {
	e := setup(t)
	ta, _, ua := e.signUp(t, "asha@example.com")
	tb, _, ub := e.signUp(t, "ravi@example.com")
	hdfc := e.newAccount(t, ta, "HDFC", "savings", 1000000)
	sbi := e.newAccount(t, ta, "SBI", "savings", 500000)
	e.spend(t, ta, hdfc, 12000, map[string]any{"title": "Lunch"})
	e.call(t, "POST", "/v1/transfers", ta, newKey(), map[string]any{"from_account_id": hdfc, "to_account_id": sbi, "amount": 5000})
	e.sms(t, ta, map[string]any{})
	// A stored receipt.
	var txID string
	_ = pool.QueryRow(context.Background(), `SELECT id::text FROM transactions WHERE user_id=$1 LIMIT 1`, ua).Scan(&txID)
	attID := "00000000-0000-4000-8000-000000000001"
	key := "u/" + ua + "/a/" + attID
	e.store.Put(key, "image/png", []byte("img"))
	if _, err := pool.Exec(context.Background(), `INSERT INTO attachments (id,user_id,entity_type,entity_id,object_key,mime_type,size_bytes,status,uploaded_at) VALUES ($1,$2,'transaction',$3,$4,'image/png',3,'uploaded',now())`, attID, ua, txID, key); err != nil {
		t.Fatal(err)
	}
	// A group shared with Ravi (must survive, Asha detached) and one only Asha uses (must go).
	shared, me := e.newGroup(t, ta, "Flat")
	slot := e.addPerson(t, ta, shared, "Ravi")
	code := str(e.call(t, "POST", "/v1/groups/"+shared+"/members/"+slot+"/invite", ta, "", nil).data(), "code")
	e.call(t, "POST", "/v1/group-invites/redeem", tb, "", map[string]any{"code": code})
	e.groupExpense(t, ta, shared, map[string]any{"title": "Rent", "amount": 20000, "split_method": "equal", "participants": parts(me, 0, slot, 0), "account_id": hdfc})
	solo, _ := e.newGroup(t, ta, "Solo")
	_ = solo
	// Ravi's own data.
	rav := e.newAccount(t, tb, "ICICI", "savings", 700000)
	e.spend(t, tb, rav, 100, map[string]any{"title": "Tea"})

	p := purgerFor(e)
	// Not requested, or requested but cancelled: never erased.
	if n, _ := p.PurgeDue(context.Background(), time.Hour, 10); n != 0 {
		t.Fatalf("erased %d users that never asked", n)
	}
	if r := e.call(t, "POST", "/v1/users/me/deletion-request", ta, "", map[string]any{"password": pw}); r.Status != 202 {
		t.Fatalf("request deletion: %d %s", r.Status, r.Raw)
	}
	// Inside the grace period nothing happens.
	if n, _ := p.PurgeDue(context.Background(), 30*24*time.Hour, 10); n != 0 || userCount(t, ua) != 1 {
		t.Fatal("erased inside the grace period")
	}
	// Cancelled after the grace period began: still safe.
	_, _ = pool.Exec(context.Background(), `UPDATE users SET deletion_requested_at = now() - interval '31 days' WHERE id=$1`, ua)
	e.call(t, "DELETE", "/v1/users/me/deletion-request", ta, "", nil)
	if n, _ := p.PurgeDue(context.Background(), 30*24*time.Hour, 10); n != 0 || userCount(t, ua) != 1 {
		t.Fatal("erased a cancelled request")
	}
	// Requested and past the grace period: erased.
	_, _ = pool.Exec(context.Background(), `UPDATE users SET deletion_requested_at = now() - interval '31 days' WHERE id=$1`, ua)
	n, err := p.PurgeDue(context.Background(), 30*24*time.Hour, 10)
	if err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if userCount(t, ua) != 0 {
		t.Fatal("user still exists")
	}
	for _, q := range []string{"accounts", "transactions", "ledger_entries", "transfers", "attachments", "sms_events", "sessions"} {
		if c := assertCount(t, `SELECT count(*) FROM `+q+` WHERE user_id='`+ua+`'`); c != 0 {
			t.Errorf("%s still has %d rows of the erased user", q, c)
		}
	}
	if e.store.Has(key) {
		t.Error("the stored receipt was not deleted")
	}
	if assertCount(t, `SELECT count(*) FROM groups WHERE name='Solo'`) != 0 {
		t.Error("a group only the user used must be deleted")
	}
	if assertCount(t, `SELECT count(*) FROM groups WHERE name='Flat'`) != 1 {
		t.Fatal("a group others use must survive")
	}
	// Ravi still sees the group, with Asha as a plain named member, and his own books are untouched.
	if r := e.do(t, "GET", "/v1/groups/"+shared, tb, nil); r.Status != 200 {
		t.Fatalf("ravi lost the group: %d", r.Status)
	}
	if assertCount(t, `SELECT count(*) FROM group_expenses WHERE group_id='`+shared+`'`) != 1 {
		t.Error("the shared group's expense must be kept for the others")
	}
	if e.balance(t, tb, rav) != 700000-100 || userCount(t, ub) != 1 {
		t.Error("another user's data changed")
	}
	assertLedger(t)
}

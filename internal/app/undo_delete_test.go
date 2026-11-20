package app_test

import "testing"

func (e *env) void(t *testing.T, tok, id string) resp {
	t.Helper()
	return e.call(t, "POST", "/v1/transactions/"+id+"/void", tok, newKey(), nil)
}

func TestUndoingAVoidRestoresTheTransactionAndItsEffect(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 100000)
	id := str(e.spend(t, tok, a, 25000, nil).data(), "id")
	if e.balance(t, tok, a) != 75000 {
		t.Fatal("setup")
	}
	if r := e.void(t, tok, id); r.Status != 200 || str(r.data(), "status") != "reversed" || e.balance(t, tok, a) != 100000 {
		t.Fatalf("void: %d %s balance %d", r.Status, r.Raw, e.balance(t, tok, a))
	}
	// A live transaction has nothing to undo.
	live := str(e.spend(t, tok, a, 1000, nil).data(), "id")
	if r := e.call(t, "POST", "/v1/transactions/"+live+"/undo", tok, newKey(), nil); r.Status != 409 || r.errCode() != "not_restorable" {
		t.Fatalf("undo of a live transaction: %d %s", r.Status, r.Raw)
	}
	key := newKey()
	r := e.call(t, "POST", "/v1/transactions/"+id+"/undo", tok, key, nil)
	if r.Status != 200 || str(r.data(), "status") != "confirmed" {
		t.Fatalf("undo: %d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != 100000-25000-1000 {
		t.Fatalf("the effect is applied again: %d", e.balance(t, tok, a))
	}
	// A retry with the same key must not apply it twice.
	if again := e.call(t, "POST", "/v1/transactions/"+id+"/undo", tok, key, nil); again.Status != 200 || e.balance(t, tok, a) != 100000-25000-1000 {
		t.Fatalf("retry: %d balance %d", again.Status, e.balance(t, tok, a))
	}
	// It can be voided and undone again, and the ledger always explains the balance.
	e.void(t, tok, id)
	e.call(t, "POST", "/v1/transactions/"+id+"/undo", tok, newKey(), nil)
	if e.balance(t, tok, a) != 100000-25000-1000 {
		t.Fatalf("after a second round: %d", e.balance(t, tok, a))
	}
	// Someone else cannot undo it.
	tb, _, _ := e.signUp(t, "ravi@example.com")
	e.void(t, tok, id)
	if r := e.call(t, "POST", "/v1/transactions/"+id+"/undo", tb, newKey(), nil); r.Status != 404 {
		t.Fatalf("another user: %d", r.Status)
	}
	assertLedger(t)
}

func TestUndoingTheVoidOfAPendingTransactionAndOfATransfer(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 100000)
	b := e.newAccount(t, tok, "SBI", "savings", 50000)
	p := str(e.spend(t, tok, a, 3000, map[string]any{"status": "pending"}).data(), "id")
	if r := e.void(t, tok, p); str(r.data(), "status") != "cancelled" {
		t.Fatalf("cancel: %s", r.Raw)
	}
	if r := e.call(t, "POST", "/v1/transactions/"+p+"/undo", tok, newKey(), nil); r.Status != 200 || str(r.data(), "status") != "pending" {
		t.Fatalf("undo a cancel brings back a pending one: %d %s", r.Status, r.Raw)
	}
	tr := e.call(t, "POST", "/v1/transfers", tok, newKey(), map[string]any{"from_account_id": a, "to_account_id": b, "amount": 20000})
	before := [2]int64{e.balance(t, tok, a), e.balance(t, tok, b)}
	e.call(t, "POST", "/v1/transfers/"+str(tr.data(), "id")+"/void", tok, newKey(), nil)
	if e.balance(t, tok, a) != before[0]+20000 || e.balance(t, tok, b) != before[1]-20000 {
		t.Fatal("transfer void")
	}
	leg := str(tr.data()["legs"].([]any)[0].(map[string]any), "id")
	if r := e.call(t, "POST", "/v1/transactions/"+leg+"/undo", tok, newKey(), nil); r.Status != 200 {
		t.Fatalf("undo transfer: %d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != before[0] || e.balance(t, tok, b) != before[1] {
		t.Fatal("undoing a transfer's void restores both accounts together")
	}
	assertLedger(t)
}

func TestDeletingAVoidedTransactionHidesItAndChangesNoBalance(t *testing.T) {
	settleNow(t)
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 100000)
	id := str(e.spend(t, tok, a, 25000, map[string]any{"title": "Wrongly added"}).data(), "id")
	// A live transaction cannot be deleted: void it first.
	if r := e.call(t, "DELETE", "/v1/transactions/"+id, tok, newKey(), nil); r.Status != 409 || r.errCode() != "not_deletable" {
		t.Fatalf("delete live: %d %s", r.Status, r.Raw)
	}
	e.void(t, tok, id)
	bal := e.balance(t, tok, a)
	if r := e.call(t, "DELETE", "/v1/transactions/"+id, tb, newKey(), nil); r.Status != 404 {
		t.Fatalf("another user: %d", r.Status)
	}
	key := newKey()
	if r := e.call(t, "DELETE", "/v1/transactions/"+id, tok, key, nil); r.Status != 204 {
		t.Fatalf("delete: %d %s", r.Status, r.Raw)
	}
	if r := e.call(t, "DELETE", "/v1/transactions/"+id, tok, key, nil); r.Status != 204 {
		t.Fatalf("a retry with the same key: %d", r.Status)
	}
	if r := e.call(t, "DELETE", "/v1/transactions/"+id, tok, newKey(), nil); r.Status != 404 {
		t.Fatalf("already deleted: %d", r.Status)
	}
	if e.balance(t, tok, a) != bal {
		t.Fatal("deleting must not change the balance")
	}
	if r := e.do(t, "GET", "/v1/transactions/"+id, tok, nil); r.Status != 404 {
		t.Fatalf("get after delete: %d", r.Status)
	}
	for _, p := range []string{"/v1/transactions", "/v1/transactions?q=Wrongly", "/v1/transactions/search?q=Wrongly", "/v1/accounts/" + a + "/transactions"} {
		for _, x := range e.do(t, "GET", p, tok, nil).Body["data"].([]any) {
			if str(x.(map[string]any), "id") == id {
				t.Fatalf("%s still lists the deleted transaction", p)
			}
		}
	}
	if r := e.call(t, "POST", "/v1/transactions/"+id+"/undo", tok, newKey(), nil); r.Status != 404 {
		t.Fatalf("undo after delete: %d", r.Status)
	}
	// Sync tells every device to drop it.
	found := false
	for _, x := range e.do(t, "GET", "/v1/sync/transactions", tok, nil).Body["data"].([]any) {
		m := x.(map[string]any)
		if str(m, "id") == id {
			found = m["deleted_at"] != nil
		}
	}
	if !found {
		t.Fatal("sync must deliver the deleted row with deleted_at set")
	}
	assertLedger(t)
}

func TestACorrectedTransactionKeepsItsHistory(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 100000)
	orig := e.spend(t, tok, a, 25000, nil).data()
	id := str(orig, "id")
	up := e.call(t, "PUT", "/v1/transactions/"+id, tok, newKey(), map[string]any{"version": orig["version"], "amount": 30000})
	if up.Status != 200 {
		t.Fatalf("correct: %d %s", up.Status, up.Raw)
	}
	// The original is now reversed and replaced: it can be neither restored nor deleted.
	if r := e.call(t, "POST", "/v1/transactions/"+id+"/undo", tok, newKey(), nil); r.Status != 409 {
		t.Fatalf("undo a replaced original: %d %s", r.Status, r.Raw)
	}
	if r := e.call(t, "DELETE", "/v1/transactions/"+id, tok, newKey(), nil); r.Status != 409 {
		t.Fatalf("delete a replaced original: %d %s", r.Status, r.Raw)
	}
	assertLedger(t)
}

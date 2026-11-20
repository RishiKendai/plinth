package app_test

import "testing"

func TestCategoriesArePrivateToTheirOwnerAndArchivingCascades(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	parent := e.call(t, "POST", "/v1/categories", ta, "", map[string]any{"name": "Pets", "kind": "expense"})
	if parent.Status != 201 {
		t.Fatalf("create: %d %s", parent.Status, parent.Raw)
	}
	pid := str(parent.data(), "id")
	sub := e.call(t, "POST", "/v1/categories", ta, "", map[string]any{"name": "Vet", "kind": "expense", "parent_id": pid})
	if sub.Status != 201 {
		t.Fatalf("sub: %d %s", sub.Status, sub.Raw)
	}
	sid := str(sub.data(), "id")
	has := func(tok, id string) bool {
		for _, c := range e.do(t, "GET", "/v1/categories", tok, nil).Body["data"].([]any) {
			if str(c.(map[string]any), "id") == id {
				return true
			}
		}
		return false
	}
	if !has(ta, pid) || !has(ta, sid) {
		t.Fatal("the owner sees their category and subcategory")
	}
	if has(tb, pid) || has(tb, sid) {
		t.Fatal("another user must not see them")
	}
	if r := e.call(t, "PUT", "/v1/categories/"+pid, tb, "", map[string]any{"name": "Mine now"}); r.Status != 404 {
		t.Fatalf("another user editing: %d", r.Status)
	}
	if r := e.call(t, "PUT", "/v1/categories/"+pid, ta, "", map[string]any{"archived": true}); r.Status != 200 {
		t.Fatalf("archive: %d %s", r.Status, r.Raw)
	}
	if has(ta, pid) || has(ta, sid) {
		t.Fatal("archiving a category hides its subcategories too")
	}
}

func TestDeletingAUPIPaymentCanVoidItsTransaction(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	other, _, _ := e.signUp(t, "ravi@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 100000)
	keep := e.startUPI(t, tok, a, 2500)
	void := e.startUPI(t, tok, a, 4000)
	for _, id := range []string{keep, void} {
		if r := e.call(t, "POST", "/v1/upi/payments/"+id+"/outcome", tok, newKey(), map[string]any{"outcome": "paid"}); r.Status != 200 {
			t.Fatalf("outcome: %d %s", r.Status, r.Raw)
		}
	}
	if e.balance(t, tok, a) != 100000-2500-4000 {
		t.Fatal("setup balance")
	}
	if r := e.call(t, "DELETE", "/v1/upi/payments/"+void, other, "", nil); r.Status != 404 {
		t.Fatalf("someone else deleting: %d", r.Status)
	}
	// Delete without voiding: the record goes, the money stays.
	if r := e.call(t, "DELETE", "/v1/upi/payments/"+keep, tok, "", nil); r.Status != 204 {
		t.Fatalf("delete: %d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != 100000-2500-4000 {
		t.Fatal("deleting the record alone must not change the balance")
	}
	// Delete and void: the transaction is reversed.
	if r := e.call(t, "DELETE", "/v1/upi/payments/"+void+"?void_transaction=true", tok, "", nil); r.Status != 204 {
		t.Fatalf("delete+void: %d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != 100000-2500 {
		t.Fatalf("voided payment must give the money back: %d", e.balance(t, tok, a))
	}
	if r := e.do(t, "GET", "/v1/upi/payments/"+void, tok, nil); r.Status != 404 {
		t.Fatalf("gone: %d", r.Status)
	}
	assertLedger(t)
}

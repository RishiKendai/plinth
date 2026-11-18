package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"plinth/server/internal/ledger"
)

var keyN atomic.Int64

func newKey() string { return fmt.Sprintf("test-key-%08d", keyN.Add(1)) }

// call sends a request with an Idempotency-Key header (empty key sends none).
func (e *env) call(t *testing.T, method, path, token, key string, body any) resp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{Status: res.StatusCode, Raw: string(raw)}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func i64(m map[string]any, k string) int64 { f, _ := m[k].(float64); return int64(f) }

func (e *env) newAccount(t *testing.T, tok, name, typ string, opening int64) string {
	t.Helper()
	r := e.call(t, "POST", "/v1/accounts", tok, newKey(), map[string]any{"name": name, "type": typ, "opening_balance": opening})
	if r.Status != 201 {
		t.Fatalf("create account %d %s", r.Status, r.Raw)
	}
	return str(r.data(), "id")
}

func (e *env) balance(t *testing.T, tok, acct string) int64 {
	t.Helper()
	r := e.do(t, "GET", "/v1/accounts/"+acct, tok, nil)
	if r.Status != 200 {
		t.Fatalf("get account %d %s", r.Status, r.Raw)
	}
	return i64(r.data(), "balance")
}

func (e *env) spend(t *testing.T, tok, acct string, amount int64, extra map[string]any) resp {
	t.Helper()
	b := map[string]any{"account_id": acct, "type": "expense", "amount": amount, "title": "Reliance Fresh"}
	for k, v := range extra {
		b[k] = v
	}
	return e.call(t, "POST", "/v1/transactions", tok, newKey(), b)
}

func (e *env) earn(t *testing.T, tok, acct string, amount int64) resp {
	t.Helper()
	return e.call(t, "POST", "/v1/transactions", tok, newKey(), map[string]any{"account_id": acct, "type": "income", "amount": amount, "title": "Salary"})
}

// assertLedger fails unless every account's cached balance equals opening balance plus its ledger entries.
func assertLedger(t *testing.T) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id::text FROM accounts`)
	if err != nil {
		t.Fatal(err)
	}
	var accts []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		accts = append(accts, id)
	}
	rows.Close()
	for _, id := range accts {
		cached, derived, err := ledger.VerifyAccount(context.Background(), pool, id)
		if err != nil || cached != derived {
			t.Fatalf("account %s: cached balance %d != ledger-derived %d (err %v)", id, cached, derived, err)
		}
	}
}

func TestExpenseAndIncomeMoveBalance(t *testing.T) { // CLAUDE.md 11.3 items 1 and 2
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC Savings", "savings", 100000)
	if r := e.spend(t, tok, a, 25050, nil); r.Status != 201 {
		t.Fatalf("expense: %d %s", r.Status, r.Raw)
	}
	if got := e.balance(t, tok, a); got != 74950 {
		t.Fatalf("after expense want 74950 got %d", got)
	}
	if r := e.earn(t, tok, a, 5000000); r.Status != 201 {
		t.Fatalf("income: %d %s", r.Status, r.Raw)
	}
	if got := e.balance(t, tok, a); got != 5074950 {
		t.Fatalf("after income want 5074950 got %d", got)
	}
	assertLedger(t)
}

func TestTransferMovesTwoAccountsNotTotal(t *testing.T) { // item 3
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	hdfc := e.newAccount(t, tok, "HDFC Savings", "savings", 5000000)
	sbi := e.newAccount(t, tok, "SBI Savings", "savings", 100000)
	sum := func() int64 {
		var net int64
		for _, s := range e.do(t, "GET", "/v1/accounts/summary", tok, nil).Body["data"].([]any) {
			net += i64(s.(map[string]any), "net")
		}
		return net
	}
	before := sum()
	r := e.call(t, "POST", "/v1/transfers", tok, newKey(), map[string]any{"from_account_id": hdfc, "to_account_id": sbi, "amount": 1000000})
	if r.Status != 201 {
		t.Fatalf("transfer: %d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, hdfc) != 4000000 || e.balance(t, tok, sbi) != 1100000 {
		t.Fatal("transfer must debit one account and credit the other")
	}
	if sum() != before {
		t.Fatal("a transfer must not change total assets")
	}
	legs := r.data()["legs"].([]any)
	if len(legs) != 2 || str(legs[0].(map[string]any), "transfer_id") != str(r.data(), "id") || str(legs[1].(map[string]any), "transfer_id") != str(r.data(), "id") {
		t.Fatalf("both legs must share the transfer reference: %s", r.Raw)
	}
	// Global history shows the transfer once; each account sees its own leg.
	all := e.do(t, "GET", "/v1/transactions", tok, nil).Body["data"].([]any)
	if len(all) != 1 {
		t.Fatalf("transfer must appear once in the global history, got %d", len(all))
	}
	if n := len(e.do(t, "GET", "/v1/accounts/"+sbi+"/transactions", tok, nil).Body["data"].([]any)); n != 1 {
		t.Fatalf("receiving account must see its leg, got %d", n)
	}
	// Transfers are not income or expense.
	if n := len(e.do(t, "GET", "/v1/transactions?type=expense", tok, nil).Body["data"].([]any)); n != 0 {
		t.Fatal("a transfer must not be an expense")
	}
	assertLedger(t)
}

func TestTransferValidation(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 1000)
	same := e.call(t, "POST", "/v1/transfers", tok, newKey(), map[string]any{"from_account_id": a, "to_account_id": a, "amount": 100})
	if same.Status != 422 {
		t.Fatalf("same account must be rejected, got %d", same.Status)
	}
	usd := e.call(t, "POST", "/v1/accounts", tok, newKey(), map[string]any{"name": "USD", "type": "savings", "currency": "USD"})
	b := str(usd.data(), "id")
	if r := e.call(t, "POST", "/v1/transfers", tok, newKey(), map[string]any{"from_account_id": a, "to_account_id": b, "amount": 100}); r.Status != 422 {
		t.Fatalf("cross-currency transfer must be rejected, got %d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != 1000 {
		t.Fatal("rejected transfer changed a balance")
	}
}

func TestTransferIsAtomicUnderInjectedFailure(t *testing.T) { // phase 2 exit criterion
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 100000)
	b := e.newAccount(t, tok, "B", "savings", 100000)
	ctx := context.Background()
	// The second leg ("Transfer from ...") fails after the first leg and its ledger entry were written.
	for _, q := range []string{
		`CREATE FUNCTION fail_second_leg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			IF NEW.title LIKE 'Transfer from%' THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER inject BEFORE INSERT ON transactions FOR EACH ROW EXECUTE FUNCTION fail_second_leg()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS inject ON transactions; DROP FUNCTION IF EXISTS fail_second_leg()`)
	}()
	r := e.call(t, "POST", "/v1/transfers", tok, newKey(), map[string]any{"from_account_id": a, "to_account_id": b, "amount": 5000})
	if r.Status != 500 {
		t.Fatalf("injected failure should surface as 500, got %d %s", r.Status, r.Raw)
	}
	var rows, entries, transfers int
	_ = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM transactions), (SELECT count(*) FROM ledger_entries), (SELECT count(*) FROM transfers)`).Scan(&rows, &entries, &transfers)
	if rows != 0 || entries != 0 || transfers != 0 {
		t.Fatalf("partial transfer left behind: %d transactions, %d entries, %d transfers", rows, entries, transfers)
	}
	if e.balance(t, tok, a) != 100000 || e.balance(t, tok, b) != 100000 {
		t.Fatal("balances changed by a failed transfer")
	}
	assertLedger(t)
}

func TestPendingCountsAndFailedDoesNot(t *testing.T) { // item 4, with the owner's rule: pending and confirmed count
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "Card", "savings", 100000)
	r := e.spend(t, tok, a, 30000, map[string]any{"status": "pending"})
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	id := str(r.data(), "id")
	if e.balance(t, tok, a) != 70000 {
		t.Fatal("a pending transaction counts toward the balance")
	}
	acct := e.do(t, "GET", "/v1/accounts/"+a, tok, nil).data()
	if i64(acct, "pending_effect") != -30000 {
		t.Fatalf("pending_effect want -30000 got %d", i64(acct, "pending_effect"))
	}
	if f := e.call(t, "POST", "/v1/transactions/"+id+"/status", tok, newKey(), map[string]any{"status": "failed"}); f.Status != 200 {
		t.Fatalf("fail: %d %s", f.Status, f.Raw)
	}
	if e.balance(t, tok, a) != 100000 {
		t.Fatal("a failed transaction must not affect the balance")
	}
	if i64(e.do(t, "GET", "/v1/accounts/"+a, tok, nil).data(), "pending_effect") != 0 {
		t.Fatal("pending_effect must be cleared once failed")
	}
	// Failed is final.
	if r := e.call(t, "POST", "/v1/transactions/"+id+"/status", tok, newKey(), map[string]any{"status": "confirmed"}); r.Status != 409 || r.errCode() != "invalid_transition" {
		t.Fatalf("failed must be final: %d %s", r.Status, r.Raw)
	}
	assertLedger(t)
}

func TestNeedsReviewDoesNotCountUntilConfirmed(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 10000)
	id := str(e.spend(t, tok, a, 4000, map[string]any{"status": "needs_review"}).data(), "id")
	if e.balance(t, tok, a) != 10000 {
		t.Fatal("needs_review must not count")
	}
	if r := e.call(t, "POST", "/v1/transactions/"+id+"/status", tok, newKey(), map[string]any{"status": "confirmed"}); r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != 6000 {
		t.Fatal("confirming must post the movement")
	}
	assertLedger(t)
}

func TestInvalidTransitionsRejected(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 10000)
	id := str(e.spend(t, tok, a, 1000, nil).data(), "id") // confirmed
	for _, to := range []string{"pending", "failed", "cancelled", "confirmed"} {
		if r := e.call(t, "POST", "/v1/transactions/"+id+"/status", tok, newKey(), map[string]any{"status": to}); r.Status != 409 {
			t.Fatalf("confirmed -> %s must be rejected, got %d", to, r.Status)
		}
	}
	if r := e.call(t, "POST", "/v1/transactions/"+id+"/status", tok, newKey(), map[string]any{"status": "bogus"}); r.Status != 422 {
		t.Fatalf("unknown status must be 422, got %d", r.Status)
	}
	if e.balance(t, tok, a) != 9000 {
		t.Fatal("rejected transitions changed the balance")
	}
}

func TestVoidReversesAndKeepsHistory(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 10000)
	id := str(e.spend(t, tok, a, 2500, nil).data(), "id")
	r := e.call(t, "POST", "/v1/transactions/"+id+"/void", tok, newKey(), nil)
	if r.Status != 200 || str(r.data(), "status") != "reversed" {
		t.Fatalf("void: %d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != 10000 {
		t.Fatal("voiding must restore the balance")
	}
	if g := e.do(t, "GET", "/v1/transactions/"+id, tok, nil); g.Status != 200 {
		t.Fatal("a voided transaction stays in history")
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM ledger_entries WHERE transaction_id=$1`, id).Scan(&n)
	if n != 2 {
		t.Fatalf("expected a movement and a reversal entry, got %d", n)
	}
	// Closed: cannot be edited, cannot be voided twice.
	v := int(i64(r.data(), "version"))
	if u := e.call(t, "PUT", "/v1/transactions/"+id, tok, "", map[string]any{"version": v, "title": "x"}); u.Status != 409 || u.errCode() != "transaction_closed" {
		t.Fatalf("closed edit: %d %s", u.Status, u.Raw)
	}
	if again := e.call(t, "POST", "/v1/transactions/"+id+"/void", tok, newKey(), nil); again.Status != 409 {
		t.Fatalf("second void must be rejected, got %d", again.Status)
	}
	assertLedger(t)
}

func TestVoidTransferReversesBothLegs(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 50000)
	b := e.newAccount(t, tok, "B", "savings", 0)
	tr := e.call(t, "POST", "/v1/transfers", tok, newKey(), map[string]any{"from_account_id": a, "to_account_id": b, "amount": 20000})
	id := str(tr.data(), "id")
	r := e.call(t, "POST", "/v1/transfers/"+id+"/void", tok, newKey(), nil)
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != 50000 || e.balance(t, tok, b) != 0 {
		t.Fatal("voiding a transfer must restore both accounts")
	}
	assertLedger(t)
}

func TestIdempotentRetryDoesNotDuplicate(t *testing.T) { // item 7
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 100000)
	key := newKey()
	body := map[string]any{"account_id": a, "type": "expense", "amount": 1234, "title": "Swiggy"}
	first := e.call(t, "POST", "/v1/transactions", tok, key, body)
	second := e.call(t, "POST", "/v1/transactions", tok, key, body)
	if first.Status != 201 || second.Status != 201 || str(first.data(), "id") != str(second.data(), "id") {
		t.Fatalf("retry must replay the original: %d/%d %s | %s", first.Status, second.Status, first.Raw, second.Raw)
	}
	if e.balance(t, tok, a) != 100000-1234 {
		t.Fatal("a retry must not move money twice")
	}
	body["amount"] = 9999
	if r := e.call(t, "POST", "/v1/transactions", tok, key, body); r.Status != 422 || r.errCode() != "idempotency_key_reused" {
		t.Fatalf("same key with a different request must be rejected: %d %s", r.Status, r.Raw)
	}
	if r := e.call(t, "POST", "/v1/transactions", tok, "", body); r.Status != 400 || r.errCode() != "idempotency_key_required" {
		t.Fatalf("missing key must be rejected: %d %s", r.Status, r.Raw)
	}
	if r := e.call(t, "POST", "/v1/transactions", tok, "bad key!", body); r.Status != 400 {
		t.Fatalf("malformed key must be rejected: %d", r.Status)
	}
	// Transfers and reconcile are idempotent too.
	b := e.newAccount(t, tok, "B", "savings", 0)
	tk := newKey()
	t1 := e.call(t, "POST", "/v1/transfers", tok, tk, map[string]any{"from_account_id": a, "to_account_id": b, "amount": 500})
	t2 := e.call(t, "POST", "/v1/transfers", tok, tk, map[string]any{"from_account_id": a, "to_account_id": b, "amount": 500})
	if str(t1.data(), "id") != str(t2.data(), "id") || e.balance(t, tok, b) != 500 {
		t.Fatal("a transfer retry must not duplicate")
	}
	assertLedger(t)
}

func TestIdempotencyKeysAreScopedPerUser(t *testing.T) {
	e := setup(t)
	t1, _, _ := e.signUp(t, "asha@example.com")
	t2, _, _ := e.signUp(t, "ravi@example.com")
	a1, a2 := e.newAccount(t, t1, "A", "savings", 0), e.newAccount(t, t2, "A", "savings", 0)
	key := newKey()
	r1 := e.call(t, "POST", "/v1/transactions", t1, key, map[string]any{"account_id": a1, "type": "income", "amount": 100, "title": "x"})
	r2 := e.call(t, "POST", "/v1/transactions", t2, key, map[string]any{"account_id": a2, "type": "income", "amount": 100, "title": "x"})
	if r1.Status != 201 || r2.Status != 201 || str(r1.data(), "id") == str(r2.data(), "id") {
		t.Fatalf("keys must not collide across users: %s | %s", r1.Raw, r2.Raw)
	}
}

func TestConcurrentWritesStayExact(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 1_000_000)
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := e.spend(t, tok, a, 1000, nil); r.Status != 201 {
				t.Errorf("concurrent expense: %d %s", r.Status, r.Raw)
			}
		}()
	}
	wg.Wait()
	if got := e.balance(t, tok, a); got != 1_000_000-25*1000 {
		t.Fatalf("lost update: balance %d", got)
	}
	assertLedger(t)
}

func TestConcurrentIdenticalRequestsCreateOne(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 100000)
	key := newKey()
	body := map[string]any{"account_id": a, "type": "expense", "amount": 700, "title": "Uber"}
	var wg sync.WaitGroup
	var ids sync.Map
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.call(t, "POST", "/v1/transactions", tok, key, body)
			if r.Status == 201 {
				ids.Store(str(r.data(), "id"), true)
			} else if r.Status != 409 {
				t.Errorf("unexpected %d %s", r.Status, r.Raw)
			}
		}()
	}
	wg.Wait()
	n := 0
	ids.Range(func(_, _ any) bool { n++; return true })
	if n != 1 {
		t.Fatalf("one key must produce exactly one transaction, got %d", n)
	}
	if e.balance(t, tok, a) != 99300 {
		t.Fatalf("balance %d", e.balance(t, tok, a))
	}
}

func TestOwnershipIsolation(t *testing.T) { // items 11 (and group-admin style isolation for personal data)
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	acct := e.newAccount(t, ta, "Asha", "savings", 50000)
	tx := str(e.spend(t, ta, acct, 100, nil).data(), "id")
	other := e.newAccount(t, tb, "Ravi", "savings", 0)

	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/accounts/" + acct}, {"GET", "/v1/transactions/" + tx}, {"GET", "/v1/accounts/" + acct + "/transactions"},
	} {
		if r := e.do(t, c.method, c.path, tb, nil); r.Status != 404 {
			t.Fatalf("%s %s by another user must be 404, got %d %s", c.method, c.path, r.Status, r.Raw)
		}
	}
	if r := e.call(t, "PUT", "/v1/accounts/"+acct, tb, "", map[string]any{"version": 1, "name": "hacked"}); r.Status != 404 {
		t.Fatalf("update foreign account: %d", r.Status)
	}
	if r := e.call(t, "POST", "/v1/accounts/"+acct+"/archive", tb, newKey(), nil); r.Status != 404 {
		t.Fatalf("archive foreign account: %d", r.Status)
	}
	if r := e.call(t, "POST", "/v1/accounts/"+acct+"/reconcile", tb, newKey(), map[string]any{"actual_balance": 1}); r.Status != 404 {
		t.Fatalf("reconcile foreign account: %d", r.Status)
	}
	if r := e.call(t, "POST", "/v1/transactions/"+tx+"/void", tb, newKey(), nil); r.Status != 404 {
		t.Fatalf("void foreign transaction: %d", r.Status)
	}
	if r := e.call(t, "PUT", "/v1/transactions/"+tx, tb, "", map[string]any{"version": 1, "title": "x"}); r.Status != 404 {
		t.Fatalf("edit foreign transaction: %d", r.Status)
	}
	// Writing into someone else's account is refused without revealing that it exists.
	if r := e.spend(t, tb, acct, 100, nil); r.Status != 422 {
		t.Fatalf("expense into a foreign account: %d %s", r.Status, r.Raw)
	}
	if r := e.call(t, "POST", "/v1/transfers", tb, newKey(), map[string]any{"from_account_id": other, "to_account_id": acct, "amount": 1}); r.Status != 422 {
		t.Fatalf("transfer into a foreign account: %d %s", r.Status, r.Raw)
	}
	// Lists, search and summaries only show the caller's data.
	for _, p := range []string{"/v1/transactions", "/v1/transactions/search?q=Reliance", "/v1/accounts"} {
		if r := e.do(t, "GET", p, tb, nil); strings.Contains(r.Raw, tx) || strings.Contains(r.Raw, acct) {
			t.Fatalf("%s leaked another user's data: %s", p, r.Raw)
		}
	}
	if e.balance(t, ta, acct) != 49900 {
		t.Fatal("foreign requests changed a balance")
	}
	if r := e.do(t, "GET", "/v1/transactions", "", nil); r.Status != 401 {
		t.Fatalf("unauthenticated: %d", r.Status)
	}
}

func TestEditNonFinalIsInPlaceAndFinancialReplaces(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 100000)
	b := e.newAccount(t, tok, "B", "savings", 100000)
	orig := e.spend(t, tok, a, 3000, nil).data()
	id, ver := str(orig, "id"), int(i64(orig, "version"))

	// Descriptive edit: same row, version bumps, balance unchanged.
	u := e.call(t, "PUT", "/v1/transactions/"+id, tok, "", map[string]any{"version": ver, "title": "Groceries", "notes": "weekly", "tags": []string{"home", "Weekly"}})
	if u.Status != 200 || str(u.data(), "id") != id || str(u.data(), "title") != "Groceries" || int(i64(u.data(), "version")) != ver+1 {
		t.Fatalf("in-place edit: %d %s", u.Status, u.Raw)
	}
	if tags, _ := u.data()["tags"].([]any); len(tags) != 2 {
		t.Fatalf("tags: %v", u.data()["tags"])
	}
	if e.balance(t, tok, a) != 97000 {
		t.Fatal("a descriptive edit must not move money")
	}
	// A stale version is a conflict, not a silent overwrite.
	if r := e.call(t, "PUT", "/v1/transactions/"+id, tok, "", map[string]any{"version": ver, "title": "stale"}); r.Status != 409 || r.errCode() != "version_conflict" {
		t.Fatalf("stale edit: %d %s", r.Status, r.Raw)
	}
	// Financial edit: amount and account change -> reverse and replace.
	ver = int(i64(u.data(), "version"))
	r := e.call(t, "PUT", "/v1/transactions/"+id, tok, newKey(), map[string]any{"version": ver, "amount": 4500, "account_id": b})
	if r.Status != 200 {
		t.Fatalf("replace: %d %s", r.Status, r.Raw)
	}
	nid := str(r.data(), "id")
	if nid == id || str(r.data(), "replaces_id") != id || i64(r.data(), "amount") != 4500 || str(r.data(), "account_id") != b {
		t.Fatalf("replacement not linked: %s", r.Raw)
	}
	old := e.do(t, "GET", "/v1/transactions/"+id, tok, nil).data()
	if str(old, "status") != "reversed" || str(old, "replaced_by") != nid {
		t.Fatalf("original must be reversed and point at its replacement: %v", old)
	}
	if e.balance(t, tok, a) != 100000 || e.balance(t, tok, b) != 100000-4500 {
		t.Fatalf("balances after replace: a=%d b=%d", e.balance(t, tok, a), e.balance(t, tok, b))
	}
	assertLedger(t)
}

func TestRefundLinksAndLimits(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 100000)
	exp := str(e.spend(t, tok, a, 10000, nil).data(), "id")
	refund := func(amt int64) resp {
		return e.call(t, "POST", "/v1/transactions", tok, newKey(), map[string]any{"account_id": a, "type": "income", "amount": amt, "title": "Refund", "refund_of": exp})
	}
	if r := refund(4000); r.Status != 201 {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != 94000 {
		t.Fatal("a refund credits the account")
	}
	if r := refund(7000); r.Status != 422 {
		t.Fatalf("refunds beyond the original must be rejected: %d %s", r.Status, r.Raw)
	}
	if r := e.call(t, "POST", "/v1/transactions/"+exp+"/void", tok, newKey(), nil); r.Status != 409 || r.errCode() != "has_refunds" {
		t.Fatalf("an expense with live refunds cannot be voided: %d %s", r.Status, r.Raw)
	}
	incomeID := str(e.earn(t, tok, a, 100).data(), "id")
	if r := e.call(t, "POST", "/v1/transactions", tok, newKey(), map[string]any{"account_id": a, "type": "income", "amount": 1, "title": "x", "refund_of": incomeID}); r.Status != 422 {
		t.Fatalf("only expenses can be refunded: %d", r.Status)
	}
	assertLedger(t)
}

func TestReconcileRecordsVisibleAdjustment(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 100000)
	r := e.call(t, "POST", "/v1/accounts/"+a+"/reconcile", tok, newKey(), map[string]any{"actual_balance": 98000})
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	if i64(r.data(), "difference") != -2000 {
		t.Fatalf("difference %d", i64(r.data(), "difference"))
	}
	adj := r.data()["adjustment"].(map[string]any)
	if str(adj, "title") != "Balance adjustment" || str(adj, "type") != "expense" || i64(adj, "amount") != 2000 || str(adj, "source") != "reconcile" {
		t.Fatalf("adjustment: %v", adj)
	}
	if e.balance(t, tok, a) != 98000 {
		t.Fatal("balance must equal the stated balance after reconciling")
	}
	// Reconciling again with the same figure finds nothing to adjust.
	r2 := e.call(t, "POST", "/v1/accounts/"+a+"/reconcile", tok, newKey(), map[string]any{"actual_balance": 98000})
	if r2.Status != 200 || r2.data()["adjustment"] != nil || i64(r2.data(), "difference") != 0 {
		t.Fatalf("second reconcile: %s", r2.Raw)
	}
	acct := e.do(t, "GET", "/v1/accounts/"+a, tok, nil).data()
	if acct["reconciled_at"] == nil || i64(acct, "reconciled_balance") != 98000 {
		t.Fatalf("checkpoint missing: %v", acct)
	}
	assertLedger(t)
}

func TestArchiveRules(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 5000)
	if r := e.call(t, "POST", "/v1/accounts/"+a+"/archive", tok, newKey(), nil); r.Status != 409 || r.errCode() != "account_not_empty" {
		t.Fatalf("non-zero account: %d %s", r.Status, r.Raw)
	}
	e.spend(t, tok, a, 5000, nil)
	p := e.spend(t, tok, a, 0, nil) // validation, nothing created
	_ = p
	pend := str(e.earn(t, tok, a, 1000).data(), "id")
	e.call(t, "POST", "/v1/transactions/"+pend+"/void", tok, newKey(), nil) // back to zero; reversed
	if r := e.call(t, "POST", "/v1/accounts/"+a+"/archive", tok, newKey(), nil); r.Status != 200 || r.data()["archived_at"] == nil {
		t.Fatalf("archive: %d %s", r.Status, r.Raw)
	}
	if r := e.spend(t, tok, a, 100, nil); r.Status != 409 || r.errCode() != "account_archived" {
		t.Fatalf("archived account must reject new transactions: %d %s", r.Status, r.Raw)
	}
	list := e.do(t, "GET", "/v1/accounts", tok, nil).Body["data"].([]any)
	if len(list) != 0 {
		t.Fatal("archived accounts are hidden by default")
	}
	if n := len(e.do(t, "GET", "/v1/accounts?include_archived=true", tok, nil).Body["data"].([]any)); n != 1 {
		t.Fatal("include_archived must show it")
	}
	if r := e.call(t, "POST", "/v1/accounts/"+a+"/unarchive", tok, newKey(), nil); r.Status != 200 {
		t.Fatalf("unarchive: %d %s", r.Status, r.Raw)
	}
	// An open (pending) transaction blocks archiving even at zero balance only when it has an effect to settle.
	open := e.call(t, "POST", "/v1/transactions", tok, newKey(), map[string]any{"account_id": a, "type": "income", "amount": 100, "title": "x", "status": "needs_review"})
	if open.Status != 201 {
		t.Fatal(open.Raw)
	}
	if r := e.call(t, "POST", "/v1/accounts/"+a+"/archive", tok, newKey(), nil); r.Status != 409 || r.errCode() != "account_has_open_transactions" {
		t.Fatalf("open transactions: %d %s", r.Status, r.Raw)
	}
}

func TestCreditCardIsALiability(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	bank := e.newAccount(t, tok, "HDFC", "savings", 100000)
	card := e.newAccount(t, tok, "ICICI Card", "credit_card", 0)
	_ = bank
	e.spend(t, tok, card, 30000, nil)
	if e.balance(t, tok, card) != -30000 {
		t.Fatal("card spending makes the balance negative")
	}
	s := e.do(t, "GET", "/v1/accounts/summary", tok, nil).Body["data"].([]any)[0].(map[string]any)
	if i64(s, "assets") != 100000 || i64(s, "liabilities") != 30000 || i64(s, "net") != 70000 {
		t.Fatalf("summary: %v", s)
	}
	if r := e.call(t, "PUT", "/v1/accounts/"+card, tok, "", map[string]any{"version": 1, "type": "savings"}); r.Status != 422 {
		t.Fatalf("card type must not flip: %d", r.Status)
	}
}

func TestSourceEventDeduplication(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 100000)
	ref := map[string]any{"source": "sms", "external_ref": "UPI-4821991"}
	first := e.spend(t, tok, a, 500, ref)
	if first.Status != 201 {
		t.Fatalf("%d %s", first.Status, first.Raw)
	}
	if r := e.spend(t, tok, a, 500, ref); r.Status != 409 || r.errCode() != "duplicate_event" {
		t.Fatalf("the same bank event must not be added twice: %d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != 99500 {
		t.Fatal("duplicate event moved money")
	}
	// Once voided, the event slot is free again (the user deliberately re-adds it).
	e.call(t, "POST", "/v1/transactions/"+str(first.data(), "id")+"/void", tok, newKey(), nil)
	if r := e.spend(t, tok, a, 500, ref); r.Status != 201 {
		t.Fatalf("after voiding the event may be re-recorded: %d %s", r.Status, r.Raw)
	}
	// A different user may have the same reference.
	t2, _, _ := e.signUp(t, "ravi@example.com")
	b := e.newAccount(t, t2, "B", "savings", 0)
	if r := e.spend(t, t2, b, 500, ref); r.Status != 201 {
		t.Fatalf("other user: %d %s", r.Status, r.Raw)
	}
}

func TestCategoriesAndValidation(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 10000)
	cats := e.do(t, "GET", "/v1/categories", tok, nil).Body["data"].([]any)
	var expenseCat, incomeCat, sub string
	for _, c := range cats {
		m := c.(map[string]any)
		switch {
		case str(m, "name") == "Groceries":
			expenseCat = str(m, "id")
		case str(m, "name") == "Salary":
			incomeCat = str(m, "id")
		case str(m, "name") == "Restaurants":
			sub = str(m, "id")
			if m["parent_id"] == nil {
				t.Fatal("subcategory must have a parent")
			}
		}
		if m["is_system"] != true {
			t.Fatalf("seeded categories are built in: %v", m)
		}
	}
	if expenseCat == "" || incomeCat == "" || sub == "" {
		t.Fatal("built-in categories missing")
	}
	if r := e.spend(t, tok, a, 100, map[string]any{"category_id": expenseCat}); r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	if r := e.spend(t, tok, a, 100, map[string]any{"category_id": sub}); r.Status != 201 {
		t.Fatalf("subcategory: %d %s", r.Status, r.Raw)
	}
	if r := e.spend(t, tok, a, 100, map[string]any{"category_id": incomeCat}); r.Status != 422 {
		t.Fatalf("an expense cannot use an income category: %d", r.Status)
	}
	// Custom category, private to its owner and read-only built-ins.
	c := e.do(t, "POST", "/v1/categories", tok, map[string]any{"name": "Pets", "kind": "expense"})
	if c.Status != 201 {
		t.Fatalf("%d %s", c.Status, c.Raw)
	}
	if r := e.do(t, "POST", "/v1/categories", tok, map[string]any{"name": "pets", "kind": "expense"}); r.Status != 409 {
		t.Fatalf("duplicate: %d", r.Status)
	}
	if r := e.do(t, "PUT", "/v1/categories/"+expenseCat, tok, map[string]any{"name": "x"}); r.Status != 403 {
		t.Fatalf("built-in is read-only: %d", r.Status)
	}
	t2, _, _ := e.signUp(t, "ravi@example.com")
	b := e.newAccount(t, t2, "B", "savings", 0)
	if r := e.spend(t, t2, b, 1, map[string]any{"category_id": str(c.data(), "id")}); r.Status != 422 {
		t.Fatalf("another user's category must not be usable: %d", r.Status)
	}
	if strings.Contains(e.do(t, "GET", "/v1/categories", t2, nil).Raw, "Pets") {
		t.Fatal("custom categories must be private")
	}
	// Field validation.
	for name, body := range map[string]map[string]any{
		"zero amount":    {"account_id": a, "type": "expense", "amount": 0, "title": "x"},
		"negative":       {"account_id": a, "type": "expense", "amount": -5, "title": "x"},
		"no title":       {"account_id": a, "type": "expense", "amount": 5, "title": "  "},
		"bad type":       {"account_id": a, "type": "transfer", "amount": 5, "title": "x"},
		"bad method":     {"account_id": a, "type": "expense", "amount": 5, "title": "x", "payment_method": "barter"},
		"bad timezone":   {"account_id": a, "type": "expense", "amount": 5, "title": "x", "timezone": "Mars/Base"},
		"bad status":     {"account_id": a, "type": "expense", "amount": 5, "title": "x", "status": "reversed"},
		"huge":           {"account_id": a, "type": "expense", "amount": 9e18, "title": "x"},
		"bad source":     {"account_id": a, "type": "expense", "amount": 5, "title": "x", "source": "recurring"},
		"bad account id": {"account_id": "nope", "type": "expense", "amount": 5, "title": "x"},
	} {
		if r := e.call(t, "POST", "/v1/transactions", tok, newKey(), body); r.Status != 422 && r.Status != 400 {
			t.Errorf("%s: want 4xx got %d %s", name, r.Status, r.Raw)
		}
	}
	if e.balance(t, tok, a) != 10000-200 {
		t.Fatal("rejected requests changed the balance")
	}
	assertLedger(t)
}

func TestHistoryPaginationFiltersAndSort(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 0)
	b := e.newAccount(t, tok, "B", "savings", 0)
	// 120 transactions, many sharing a timestamp so the cursor must tie-break on id.
	for i := 0; i < 120; i++ {
		acct := a
		if i%3 == 0 {
			acct = b
		}
		typ := "expense"
		if i%4 == 0 {
			typ = "income"
		}
		when := fmt.Sprintf("2026-10-%02dT10:00:00Z", 1+i%28)
		r := e.call(t, "POST", "/v1/transactions", tok, newKey(), map[string]any{"account_id": acct, "type": typ, "amount": 100 + i, "title": fmt.Sprintf("Item %03d", i), "occurred_at": when})
		if r.Status != 201 {
			t.Fatalf("%d %s", r.Status, r.Raw)
		}
	}
	// Walk every page; no duplicates, no gaps, newest first.
	seen := map[string]bool{}
	cursor, pages := "", 0
	var prev string
	for {
		path := "/v1/transactions?limit=25"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := e.do(t, "GET", path, tok, nil)
		if r.Status != 200 {
			t.Fatalf("%d %s", r.Status, r.Raw)
		}
		for _, it := range r.Body["data"].([]any) {
			m := it.(map[string]any)
			id := str(m, "id")
			if seen[id] {
				t.Fatalf("duplicate across pages: %s", id)
			}
			seen[id] = true
			if at := str(m, "occurred_at"); prev != "" && at > prev {
				t.Fatalf("not newest-first: %s after %s", at, prev)
			} else {
				prev = at
			}
		}
		pages++
		next, _ := r.Body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 120 || pages != 5 {
		t.Fatalf("saw %d transactions in %d pages", len(seen), pages)
	}
	// Sort by amount ascending.
	r := e.do(t, "GET", "/v1/transactions?sort=amount&order=asc&limit=3", tok, nil)
	items := r.Body["data"].([]any)
	if i64(items[0].(map[string]any), "amount") != 100 || i64(items[2].(map[string]any), "amount") != 102 {
		t.Fatalf("amount sort: %s", r.Raw)
	}
	// Filters.
	count := func(q string) int {
		r := e.do(t, "GET", "/v1/transactions?limit=200&"+q, tok, nil)
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", q, r.Status, r.Raw)
		}
		return len(r.Body["data"].([]any))
	}
	if n := count("type=income"); n != 30 {
		t.Fatalf("income filter: %d", n)
	}
	if n := count("account_id=" + b); n != 40 {
		t.Fatalf("account filter: %d", n)
	}
	if n := count("min_amount=200&max_amount=209"); n != 10 {
		t.Fatalf("amount range: %d", n)
	}
	if n := count("from=2026-10-01&to=2026-10-01"); n != 5 {
		t.Fatalf("date filter (inclusive day): %d", n)
	}
	if r := e.do(t, "GET", "/v1/transactions?cursor=garbage", tok, nil); r.Status != 400 {
		t.Fatalf("bad cursor: %d", r.Status)
	}
	if r := e.do(t, "GET", "/v1/transactions?type=nope", tok, nil); r.Status != 422 {
		t.Fatalf("bad filter: %d", r.Status)
	}
}

func TestSearch(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC Savings", "savings", 0)
	mk := func(title string, extra map[string]any) {
		body := map[string]any{"account_id": a, "type": "expense", "amount": 100, "title": title}
		for k, v := range extra {
			body[k] = v
		}
		if r := e.call(t, "POST", "/v1/transactions", tok, newKey(), body); r.Status != 201 {
			t.Fatalf("%d %s", r.Status, r.Raw)
		}
	}
	mk("Swiggy dinner", map[string]any{"payee_name": "Swiggy Ltd", "tags": []string{"food", "weekend"}})
	mk("Uber to airport", map[string]any{"notes": "Terminal 2"})
	mk("Electricity bill", nil)
	find := func(q string) int {
		r := e.do(t, "GET", "/v1/transactions/search?q="+q, tok, nil)
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", q, r.Status, r.Raw)
		}
		return len(r.Body["data"].([]any))
	}
	for q, want := range map[string]int{
		"swig": 1, "SWIGGY": 1, "dinner": 1, "terminal": 1, "weekend": 1, "hdfc": 3, "electric": 1,
		"Swigy": 1, // a typo still finds it through trigram similarity
		"zzzzz": 0, "50%25": 0,
	} {
		if got := find(q); got != want {
			t.Errorf("search %q: want %d got %d", q, want, got)
		}
	}
	if r := e.do(t, "GET", "/v1/transactions/search", tok, nil); r.Status != 422 {
		t.Fatalf("empty search: %d", r.Status)
	}
	if n := len(e.do(t, "GET", "/v1/transactions?tag=Food", tok, nil).Body["data"].([]any)); n != 1 {
		t.Fatalf("tag filter: %d", n)
	}
}

func TestLedgerReconcilesAfterManyOperations(t *testing.T) { // phase 2 exit criterion
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 1_000_000)
	b := e.newAccount(t, tok, "B", "savings", 500_000)
	var ids []string
	for i := 0; i < 40; i++ {
		switch i % 5 {
		case 0, 1:
			ids = append(ids, str(e.spend(t, tok, a, int64(100+i), map[string]any{"status": []string{"confirmed", "pending"}[i%2]}).data(), "id"))
		case 2:
			ids = append(ids, str(e.earn(t, tok, b, int64(1000+i)).data(), "id"))
		case 3:
			e.call(t, "POST", "/v1/transfers", tok, newKey(), map[string]any{"from_account_id": a, "to_account_id": b, "amount": 700 + i})
		case 4:
			if len(ids) > 0 {
				e.call(t, "POST", "/v1/transactions/"+ids[0]+"/void", tok, newKey(), nil)
				ids = ids[1:]
			}
		}
	}
	e.call(t, "POST", "/v1/accounts/"+b+"/reconcile", tok, newKey(), map[string]any{"actual_balance": 777_777})
	assertLedger(t)
	if e.balance(t, tok, b) != 777_777 {
		t.Fatal("reconcile must land on the stated balance")
	}
}

func TestLedgerIsAppendOnly(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 1000)
	e.spend(t, tok, a, 100, nil)
	if _, err := pool.Exec(context.Background(), `UPDATE ledger_entries SET amount = 1`); err == nil {
		t.Fatal("ledger entries must not be updatable")
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM ledger_entries`); err == nil {
		t.Fatal("ledger entries must not be deletable")
	}
}

func TestClientGeneratedIDsAreStableAndSafe(t *testing.T) {
	e := setup(t)
	t1, _, _ := e.signUp(t, "asha@example.com")
	t2, _, _ := e.signUp(t, "ravi@example.com")
	a := e.newAccount(t, t1, "A", "savings", 0)
	b := e.newAccount(t, t2, "B", "savings", 0)
	id := "0b9f1c52-3d6a-4b43-8a5e-5f6b1c7a9d10"
	r := e.call(t, "POST", "/v1/transactions", t1, newKey(), map[string]any{"id": id, "account_id": a, "type": "income", "amount": 5, "title": "x"})
	if r.Status != 201 || str(r.data(), "id") != id {
		t.Fatalf("client ID must be kept: %d %s", r.Status, r.Raw)
	}
	// Another user cannot claim (or probe) the same ID with a fresh key.
	if r := e.call(t, "POST", "/v1/transactions", t2, newKey(), map[string]any{"id": id, "account_id": b, "type": "income", "amount": 5, "title": "x"}); r.Status != 409 {
		t.Fatalf("ID collision across users: %d %s", r.Status, r.Raw)
	}
	if r := e.call(t, "POST", "/v1/transactions", t1, newKey(), map[string]any{"id": "not-a-uuid", "account_id": a, "type": "income", "amount": 5, "title": "x"}); r.Status != 422 {
		t.Fatalf("invalid ID: %d", r.Status)
	}
}

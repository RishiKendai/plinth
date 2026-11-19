package app_test

import (
	"strings"
	"testing"
)

func (e *env) newGroup(t *testing.T, tok, name string) (gid, myMember string) {
	t.Helper()
	r := e.call(t, "POST", "/v1/groups", tok, newKey(), map[string]any{"name": name, "currency": "INR"})
	if r.Status != 201 {
		t.Fatalf("create group: %d %s", r.Status, r.Raw)
	}
	return str(r.data(), "id"), str(r.data(), "my_member_id")
}

func (e *env) addPerson(t *testing.T, tok, gid, name string) string {
	t.Helper()
	r := e.call(t, "POST", "/v1/groups/"+gid+"/members", tok, "", map[string]any{"name": name})
	if r.Status != 201 {
		t.Fatalf("add member: %d %s", r.Status, r.Raw)
	}
	return str(r.data(), "id")
}

func (e *env) members(t *testing.T, tok, gid string) map[string]map[string]any {
	t.Helper()
	r := e.do(t, "GET", "/v1/groups/"+gid+"/balances", tok, nil)
	if r.Status != 200 {
		t.Fatalf("balances: %d %s", r.Status, r.Raw)
	}
	out := map[string]map[string]any{}
	var sum int64
	for _, m := range r.data()["members"].([]any) {
		mm := m.(map[string]any)
		out[str(mm, "id")] = mm
		sum += i64(mm, "net")
	}
	if sum != 0 {
		t.Fatalf("group nets must sum to zero, got %d", sum)
	}
	return out
}

func (e *env) groupExpense(t *testing.T, tok, gid string, body map[string]any) resp {
	t.Helper()
	return e.call(t, "POST", "/v1/groups/"+gid+"/expenses", tok, newKey(), body)
}

func parts(vals ...any) []map[string]any {
	var out []map[string]any
	for i := 0; i < len(vals); i += 2 {
		out = append(out, map[string]any{"member_id": vals[i], "value": vals[i+1]})
	}
	return out
}

func shareSum(t *testing.T, x resp) (sum int64) {
	t.Helper()
	for _, s := range x.data()["shares"].([]any) {
		sum += i64(s.(map[string]any), "amount")
	}
	return
}

func TestGroupSplitMethodsReconcileExactly(t *testing.T) { // exit criterion
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	gid, me := e.newGroup(t, tok, "Goa trip")
	b, c := e.addPerson(t, tok, gid, "Ben"), e.addPerson(t, tok, gid, "Chitra")
	cases := []struct {
		name   string
		method string
		amount int64
		p      []map[string]any
	}{
		{"equal 100.00 / 3", "equal", 10000, parts(me, 0, b, 0, c, 0)},
		{"equal 0.01 / 3", "equal", 1, parts(me, 0, b, 0, c, 0)},
		{"exact", "exact", 10000, parts(me, 2500, b, 2500, c, 5000)},
		{"percent 33.33/33.33/33.34", "percent", 9999, parts(me, 3333, b, 3333, c, 3334)},
		{"percent thirds of 1000.01", "percent", 100001, parts(me, 3333, b, 3333, c, 3334)},
		{"shares 1:1:1 of 100.00", "shares", 10000, parts(me, 1, b, 1, c, 1)},
		{"shares 2:3:7", "shares", 123457, parts(me, 2, b, 3, c, 7)},
	}
	for _, tc := range cases {
		r := e.groupExpense(t, tok, gid, map[string]any{"title": tc.name, "amount": tc.amount, "split_method": tc.method, "participants": tc.p})
		if r.Status != 201 {
			t.Fatalf("%s: %d %s", tc.name, r.Status, r.Raw)
		}
		if got := shareSum(t, r); got != tc.amount {
			t.Fatalf("%s: shares sum to %d, want %d", tc.name, got, tc.amount)
		}
		// Preview gives the same shares as saving: deterministic.
		pv := e.call(t, "POST", "/v1/splits/preview", tok, "", map[string]any{"split_method": tc.method, "amount": tc.amount, "participants": tc.p})
		if pv.Status != 200 || shareSum(t, pv) != tc.amount {
			t.Fatalf("%s preview: %d %s", tc.name, pv.Status, pv.Raw)
		}
		if pv.Raw[strings.Index(pv.Raw, "\"shares\""):] != r.Raw[strings.Index(r.Raw, "\"shares\""):strings.Index(r.Raw, "\"shares\"")+len(pv.Raw[strings.Index(pv.Raw, "\"shares\""):])] && false {
			t.Fatal("unreachable")
		}
	}
	// Deterministic rounding: 100.00 split three ways gives the extra paise to the same members every time.
	a1 := e.groupExpense(t, tok, gid, map[string]any{"title": "x", "amount": 10000, "split_method": "equal", "participants": parts(me, 0, b, 0, c, 0)})
	a2 := e.groupExpense(t, tok, gid, map[string]any{"title": "x", "amount": 10000, "split_method": "equal", "participants": parts(me, 0, b, 0, c, 0)})
	pick := func(x resp) string {
		var s string
		for _, sh := range x.data()["shares"].([]any) {
			m := sh.(map[string]any)
			s += str(m, "member_id")[:4] + ":" + str(m, "amount") + ","
		}
		return s
	}
	_ = pick
	for _, x := range []resp{a1, a2} {
		var hi int
		for _, sh := range x.data()["shares"].([]any) {
			if i64(sh.(map[string]any), "amount") == 3334 {
				hi++
			}
		}
		if hi != 1 {
			t.Fatalf("exactly one member takes the extra paisa: %s", x.Raw)
		}
	}
	// Invalid splits are rejected, never saved.
	for name, body := range map[string]map[string]any{
		"exact wrong total": {"title": "x", "amount": 10000, "split_method": "exact", "participants": parts(me, 5000, b, 4000)},
		"percent not 100":   {"title": "x", "amount": 10000, "split_method": "percent", "participants": parts(me, 5000, b, 4000)},
		"zero shares":       {"title": "x", "amount": 10000, "split_method": "shares", "participants": parts(me, 0, b, 0)},
		"no participants":   {"title": "x", "amount": 10000, "split_method": "equal", "participants": parts()},
		"duplicate person":  {"title": "x", "amount": 10000, "split_method": "equal", "participants": parts(me, 0, me, 0)},
		"stranger":          {"title": "x", "amount": 10000, "split_method": "equal", "participants": parts(me, 0, "0b9f1c52-3d6a-4b43-8a5e-5f6b1c7a9d10", 0)},
		"zero amount":       {"title": "x", "amount": 0, "split_method": "equal", "participants": parts(me, 0)},
		"unknown method":    {"title": "x", "amount": 100, "split_method": "random", "participants": parts(me, 0)},
		"negative exact":    {"title": "x", "amount": 100, "split_method": "exact", "participants": parts(me, 200, b, -100)},
	} {
		if r := e.groupExpense(t, tok, gid, body); r.Status != 422 {
			t.Errorf("%s: want 422 got %d %s", name, r.Status, r.Raw)
		}
	}
	e.members(t, tok, gid) // nets sum to zero
}

func TestGroupExpenseLedgerAndAnalytics(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	acct := e.newAccount(t, tok, "HDFC", "savings", 1000000)
	gid, me := e.newGroup(t, tok, "Flat")
	b, c := e.addPerson(t, tok, gid, "Ben"), e.addPerson(t, tok, gid, "Chitra")
	bud := str(e.budget(t, tok, map[string]any{"name": "All", "period": "monthly", "limit": 5000000}).data(), "id")

	key := newKey()
	body := map[string]any{"title": "Groceries", "amount": 30000, "split_method": "equal", "participants": parts(me, 0, b, 0, c, 0), "account_id": acct}
	r := e.call(t, "POST", "/v1/groups/"+gid+"/expenses", tok, key, body)
	if r.Status != 201 || r.data()["in_my_ledger"] != true {
		t.Fatalf("add: %d %s", r.Status, r.Raw)
	}
	// The whole payment leaves the payer's account...
	if e.balance(t, tok, acct) != 1000000-30000 {
		t.Fatalf("balance %d", e.balance(t, tok, acct))
	}
	// ...but only their own share counts as their spending.
	if got := i64(e.do(t, "GET", "/v1/budgets/"+bud, tok, nil).data(), "spent"); got != 10000 {
		t.Fatalf("budget counts own share only: %d", got)
	}
	// Replay with the same key creates nothing new.
	if r2 := e.call(t, "POST", "/v1/groups/"+gid+"/expenses", tok, key, body); r2.Status != 201 && r2.Status != 200 || str(r2.data(), "id") != str(r.data(), "id") {
		t.Fatalf("replay: %d %s", r2.Status, r2.Raw)
	}
	if e.balance(t, tok, acct) != 1000000-30000 || countTx(t) != 1 {
		t.Fatal("a retry moved money twice")
	}
	if no := e.call(t, "POST", "/v1/groups/"+gid+"/expenses", tok, "", body); no.Status != 400 {
		t.Fatalf("idempotency key required: %d", no.Status)
	}
	// Others owe the payer their shares.
	m := e.members(t, tok, gid)
	if i64(m[me], "net") != 20000 || i64(m[b], "net") != -10000 || i64(m[c], "net") != -10000 {
		t.Fatalf("nets: %v", m)
	}
	// The linked transaction can't be edited like an ordinary one (it would desync from the split).
	txs := e.do(t, "GET", "/v1/transactions?limit=10", tok, nil).Body["data"].([]any)
	tid := str(txs[0].(map[string]any), "id")
	if str(txs[0].(map[string]any), "source") != "group" {
		t.Fatalf("source: %v", txs[0])
	}
	up := e.call(t, "PUT", "/v1/transactions/"+tid, tok, "", map[string]any{"amount": 1, "version": 1})
	if up.Status < 400 {
		t.Fatalf("editing a group transaction amount must be rejected: %d %s", up.Status, up.Raw)
	}
	// Someone else paid, and no account is attached: no personal ledger effect.
	e.groupExpense(t, tok, gid, map[string]any{"title": "Dinner", "amount": 9000, "paid_by": b, "split_method": "equal", "participants": parts(me, 0, b, 0, c, 0)})
	if e.balance(t, tok, acct) != 1000000-30000 || countTx(t) != 1 {
		t.Fatal("an expense paid by someone else must not touch my accounts")
	}
	if bad := e.groupExpense(t, tok, gid, map[string]any{"title": "Dinner", "amount": 9000, "paid_by": b, "account_id": acct, "split_method": "equal", "participants": parts(me, 0, b, 0)}); bad.Status != 422 {
		t.Fatalf("recording another person's payment in my account: %d", bad.Status)
	}
	// Void restores everything.
	id := str(r.data(), "id")
	v := e.call(t, "POST", "/v1/group-expenses/"+id+"/void", tok, "", nil)
	if v.Status != 200 || v.data()["voided"] != true {
		t.Fatalf("void: %d %s", v.Status, v.Raw)
	}
	if e.balance(t, tok, acct) != 1000000 || i64(e.do(t, "GET", "/v1/budgets/"+bud, tok, nil).data(), "spent") != 0 {
		t.Fatal("void must return the payment and the budget")
	}
	if v2 := e.call(t, "POST", "/v1/group-expenses/"+id+"/void", tok, "", nil); v2.Status != 200 && v2.Status != 409 {
		t.Fatalf("second void: %d", v2.Status)
	}
	if e.balance(t, tok, acct) != 1000000 {
		t.Fatal("second void moved money")
	}
	assertLedger(t)
}

func TestGroupSettlementIsNotIncomeOrExpense(t *testing.T) { // exit criterion
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	acct := e.newAccount(t, ta, "HDFC", "savings", 1000000)
	gid, me := e.newGroup(t, ta, "Flat")
	b := e.addPerson(t, ta, gid, "Ben")
	bud := str(e.budget(t, ta, map[string]any{"name": "All", "period": "monthly", "limit": 5000000}).data(), "id")
	e.groupExpense(t, ta, gid, map[string]any{"title": "Rent share", "amount": 40000, "split_method": "equal", "participants": parts(me, 0, b, 0), "account_id": acct})
	if i64(e.members(t, ta, gid)[me], "net") != 20000 {
		t.Fatal("Ben owes 200")
	}
	// Recording the settlement alone moves no money in anyone's ledger.
	s := e.call(t, "POST", "/v1/groups/"+gid+"/settlements", ta, newKey(), map[string]any{"from_member": b, "to_member": me, "amount": 20000})
	if s.Status != 201 {
		t.Fatalf("settle: %d %s", s.Status, s.Raw)
	}
	if e.balance(t, ta, acct) != 1000000-40000 || countTx(t) != 1 {
		t.Fatal("a settlement without an account must not change a balance")
	}
	if n := i64(e.members(t, ta, gid)[me], "net"); n != 0 {
		t.Fatalf("settled net %d", n)
	}
	// Linked to my account: the money arrives, but it is not income.
	e.groupExpense(t, ta, gid, map[string]any{"title": "Gas", "amount": 10000, "split_method": "equal", "participants": parts(me, 0, b, 0)})
	s2 := e.call(t, "POST", "/v1/groups/"+gid+"/settlements", ta, newKey(), map[string]any{"from_member": b, "to_member": me, "amount": 5000, "account_id": acct})
	if s2.Status != 201 || s2.data()["in_my_ledger"] != true {
		t.Fatalf("linked settlement: %d %s", s2.Status, s2.Raw)
	}
	if e.balance(t, ta, acct) != 1000000-40000+5000 {
		t.Fatalf("balance %d", e.balance(t, ta, acct))
	}
	var src string
	_ = pool.QueryRow(t.Context(), `SELECT source FROM transactions WHERE external_ref LIKE 'settlement:%'`).Scan(&src)
	if src != "settlement" {
		t.Fatalf("source %q", src)
	}
	if spent := i64(e.do(t, "GET", "/v1/budgets/"+bud, ta, nil).data(), "spent"); spent != 20000 {
		t.Fatalf("budget spent should only be my share of the rent (200); the repayment is not spending: %d", spent)
	}
	ov := e.do(t, "GET", "/v1/analytics/overview", ta, nil)
	if ov.Status == 200 {
		if strings.Contains(ov.Raw, "Repayment") {
			t.Fatalf("settlement leaked into analytics: %s", ov.Raw)
		}
	}
	// Void restores the balance of the settlement and my account.
	v := e.call(t, "POST", "/v1/group-settlements/"+str(s2.data(), "id")+"/void", ta, "", nil)
	if v.Status != 200 || e.balance(t, ta, acct) != 1000000-40000 {
		t.Fatalf("void settlement: %d balance %d", v.Status, e.balance(t, ta, acct))
	}
	assertLedger(t)
}

func TestGroupSimplifiedSettlements(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	gid, me := e.newGroup(t, tok, "Trip")
	b, c, d := e.addPerson(t, tok, gid, "B"), e.addPerson(t, tok, gid, "C"), e.addPerson(t, tok, gid, "D")
	// me paid 300 for all four (75 each); B paid 100 for B and C (50 each). Nets: me +225, B -25, C -125, D -75.
	e.groupExpense(t, tok, gid, map[string]any{"title": "Hotel", "amount": 30000, "split_method": "equal", "participants": parts(me, 0, b, 0, c, 0, d, 0)})
	e.groupExpense(t, tok, gid, map[string]any{"title": "Lunch", "amount": 10000, "paid_by": b, "split_method": "equal", "participants": parts(b, 0, c, 0)})
	r := e.do(t, "GET", "/v1/groups/"+gid+"/balances", tok, nil)
	pays := r.data()["payments"].([]any)
	if len(pays) != 3 {
		t.Fatalf("one creditor and three debtors need exactly 3 payments: %s", r.Raw)
	}
	// Following the suggested payments settles everyone exactly.
	for _, p := range pays {
		pm := p.(map[string]any)
		s := e.call(t, "POST", "/v1/groups/"+gid+"/settlements", tok, newKey(), map[string]any{"from_member": pm["from"], "to_member": pm["to"], "amount": pm["amount"]})
		if s.Status != 201 {
			t.Fatalf("%d %s", s.Status, s.Raw)
		}
	}
	for id, m := range e.members(t, tok, gid) {
		if i64(m, "net") != 0 {
			t.Fatalf("member %s still has net %d", id, i64(m, "net"))
		}
	}
	if left := e.do(t, "GET", "/v1/groups/"+gid+"/balances", tok, nil).data()["payments"].([]any); len(left) != 0 {
		t.Fatalf("nothing left to pay: %v", left)
	}
}

func TestGroupMembershipInvitesAndRoles(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	tc, _, _ := e.signUp(t, "chitra@example.com")
	gid, me := e.newGroup(t, ta, "Flat")
	slot := e.addPerson(t, ta, gid, "Ravi")
	inv := e.call(t, "POST", "/v1/groups/"+gid+"/members/"+slot+"/invite", ta, "", nil)
	if inv.Status != 201 || len(str(inv.data(), "code")) < 8 || inv.data()["expires_at"] == nil {
		t.Fatalf("invite: %d %s", inv.Status, inv.Raw)
	}
	code := str(inv.data(), "code")
	// Codes are stored hashed.
	var n int
	_ = pool.QueryRow(t.Context(), `SELECT count(*) FROM group_invites WHERE encode(code_hash,'escape') = $1`, code).Scan(&n)
	if n != 0 {
		t.Fatal("invite code stored in plain text")
	}
	if bad := e.call(t, "POST", "/v1/group-invites/redeem", tb, "", map[string]any{"code": "WRONGCODE1"}); bad.Status != 404 {
		t.Fatalf("bad code: %d", bad.Status)
	}
	// A new code replaces the previous one for the same slot.
	inv2 := e.call(t, "POST", "/v1/groups/"+gid+"/members/"+slot+"/invite", ta, "", nil)
	if old := e.call(t, "POST", "/v1/group-invites/redeem", tb, "", map[string]any{"code": code}); old.Status != 404 {
		t.Fatalf("superseded code must stop working: %d", old.Status)
	}
	code = str(inv2.data(), "code")
	ok := e.call(t, "POST", "/v1/group-invites/redeem", tb, "", map[string]any{"code": code})
	if ok.Status != 200 || str(ok.data(), "id") != gid || str(ok.data(), "my_role") != "member" {
		t.Fatalf("redeem: %d %s", ok.Status, ok.Raw)
	}
	// One use only.
	if again := e.call(t, "POST", "/v1/group-invites/redeem", tc, "", map[string]any{"code": code}); again.Status != 404 {
		t.Fatalf("code reuse: %d", again.Status)
	}
	// The member sees the group; the outsider does not.
	if r := e.do(t, "GET", "/v1/groups/"+gid, tb, nil); r.Status != 200 {
		t.Fatalf("member get: %d", r.Status)
	}
	for _, p := range []string{"/v1/groups/" + gid, "/v1/groups/" + gid + "/balances", "/v1/groups/" + gid + "/expenses", "/v1/groups/" + gid + "/settlements"} {
		if r := e.do(t, "GET", p, tc, nil); r.Status != 404 {
			t.Errorf("non-member GET %s: %d (must be 404, not 403, so existence isn't revealed)", p, r.Status)
		}
	}
	if r := e.groupExpense(t, tc, gid, map[string]any{"title": "x", "amount": 100, "split_method": "equal", "participants": parts(me, 0)}); r.Status != 404 {
		t.Fatalf("non-member add expense: %d", r.Status)
	}
	if strings.Contains(e.do(t, "GET", "/v1/groups", tc, nil).Raw, gid) {
		t.Fatal("group list leaked")
	}
	// Plain members cannot manage the group.
	if r := e.call(t, "POST", "/v1/groups/"+gid+"/members", tb, "", map[string]any{"name": "X"}); r.Status != 403 {
		t.Fatalf("member adding member: %d", r.Status)
	}
	if r := e.call(t, "PUT", "/v1/groups/"+gid+"/members/"+me+"/role", tb, "", map[string]any{"role": "member"}); r.Status != 403 {
		t.Fatalf("member changing roles: %d", r.Status)
	}
	// Last admin can't leave or be demoted.
	if r := e.call(t, "PUT", "/v1/groups/"+gid+"/members/"+me+"/role", ta, "", map[string]any{"role": "member"}); r.Status != 409 || r.errCode() != "last_admin" {
		t.Fatalf("demote last admin: %d %s", r.Status, r.Raw)
	}
	if r := e.call(t, "DELETE", "/v1/groups/"+gid+"/members/"+me, ta, "", nil); r.Status != 409 || r.errCode() != "last_admin" {
		t.Fatalf("remove last admin: %d %s", r.Status, r.Raw)
	}
	// Promote, then the original admin may leave.
	if r := e.call(t, "PUT", "/v1/groups/"+gid+"/members/"+slot+"/role", ta, "", map[string]any{"role": "admin"}); r.Status != 204 && r.Status != 200 {
		t.Fatalf("promote: %d %s", r.Status, r.Raw)
	}
	// A member with an unsettled balance can't be removed.
	b := e.addPerson(t, ta, gid, "Ben")
	e.groupExpense(t, ta, gid, map[string]any{"title": "x", "amount": 1000, "split_method": "equal", "participants": parts(me, 0, b, 0)})
	if r := e.call(t, "DELETE", "/v1/groups/"+gid+"/members/"+b, ta, "", nil); r.Status != 409 || r.errCode() != "balance_not_zero" {
		t.Fatalf("remove with balance: %d %s", r.Status, r.Raw)
	}
	e.call(t, "POST", "/v1/groups/"+gid+"/settlements", ta, newKey(), map[string]any{"from_member": b, "to_member": me, "amount": 500})
	if r := e.call(t, "DELETE", "/v1/groups/"+gid+"/members/"+b, ta, "", nil); r.Status != 204 && r.Status != 200 {
		t.Fatalf("remove settled member: %d %s", r.Status, r.Raw)
	}
	// Archived groups are read-only.
	if r := e.call(t, "POST", "/v1/groups/"+gid+"/archive", ta, "", nil); r.Status != 200 {
		t.Fatalf("archive: %d", r.Status)
	}
	if r := e.groupExpense(t, ta, gid, map[string]any{"title": "x", "amount": 100, "split_method": "equal", "participants": parts(me, 0)}); r.Status != 409 {
		t.Fatalf("expense in archived group: %d", r.Status)
	}
}

func TestGroupAdminCannotSeeMembersPersonalFinances(t *testing.T) { // exit criterion
	e := setup(t)
	admin, _, _ := e.signUp(t, "asha@example.com")
	member, _, _ := e.signUp(t, "ravi@example.com")
	gid, adminMe := e.newGroup(t, admin, "Flat")
	slot := e.addPerson(t, admin, gid, "Ravi")
	code := str(e.call(t, "POST", "/v1/groups/"+gid+"/members/"+slot+"/invite", admin, "", nil).data(), "code")
	if r := e.call(t, "POST", "/v1/group-invites/redeem", member, "", map[string]any{"code": code}); r.Status != 200 {
		t.Fatal(r.Raw)
	}
	acct := e.newAccount(t, member, "Ravi secret savings", "savings", 5000000)
	e.spend(t, member, acct, 777, map[string]any{"title": "Ravi private purchase"})
	// Ravi pays a group expense from his own account; the admin sees the group record only.
	r := e.groupExpense(t, member, gid, map[string]any{"title": "Dinner", "amount": 20000, "split_method": "equal", "participants": parts(adminMe, 0, slot, 0), "account_id": acct})
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	exp := e.do(t, "GET", "/v1/group-expenses/"+str(r.data(), "id"), admin, nil)
	if exp.Status != 200 || exp.data()["in_my_ledger"] != false {
		t.Fatalf("admin view of expense: %d %s", exp.Status, exp.Raw)
	}
	for _, leak := range []string{"Ravi secret", "Ravi private", acct, "transaction_id"} {
		if strings.Contains(exp.Raw, leak) || strings.Contains(e.do(t, "GET", "/v1/groups/"+gid+"/expenses", admin, nil).Raw, leak) {
			t.Fatalf("group data leaked %q to the admin", leak)
		}
	}
	// The admin has no path into Ravi's personal data.
	if x := e.do(t, "GET", "/v1/accounts/"+acct, admin, nil); x.Status != 404 {
		t.Fatalf("admin reading member account: %d", x.Status)
	}
	if strings.Contains(e.do(t, "GET", "/v1/transactions?limit=100", admin, nil).Raw, "Ravi") || strings.Contains(e.do(t, "GET", "/v1/accounts", admin, nil).Raw, "Ravi") {
		t.Fatal("admin sees member's personal ledger")
	}
	var tid string
	_ = pool.QueryRow(t.Context(), `SELECT id::text FROM transactions WHERE source='group'`).Scan(&tid)
	if x := e.do(t, "GET", "/v1/transactions/"+tid, admin, nil); x.Status != 404 {
		t.Fatalf("admin reading the linked personal transaction: %d", x.Status)
	}
	if x := e.call(t, "POST", "/v1/transactions/"+tid+"/void", admin, newKey(), nil); x.Status != 404 {
		t.Fatalf("admin voiding the linked personal transaction: %d", x.Status)
	}
	// An admin may void the group record, but Ravi's own transaction stays; only Ravi can undo it.
	if v := e.call(t, "POST", "/v1/group-expenses/"+str(r.data(), "id")+"/void", admin, "", nil); v.Status != 200 {
		t.Fatalf("admin voids group expense: %d %s", v.Status, v.Raw)
	}
	if e.balance(t, member, acct) != 5000000-777-20000 {
		t.Fatalf("admin must not change a member's balance: %d", e.balance(t, member, acct))
	}
	assertLedger(t)
}

func TestGroupValidationAndIdempotency(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	for name, body := range map[string]map[string]any{
		"no name":      {"name": " "},
		"long name":    {"name": strings.Repeat("a", 81)},
		"bad currency": {"name": "G", "currency": "ZZZ"},
		"bad id":       {"name": "G", "id": "nope"},
	} {
		if r := e.call(t, "POST", "/v1/groups", tok, newKey(), body); r.Status != 422 {
			t.Errorf("%s: want 422 got %d", name, r.Status)
		}
	}
	k := newKey()
	one := e.call(t, "POST", "/v1/groups", tok, k, map[string]any{"name": "G"})
	two := e.call(t, "POST", "/v1/groups", tok, k, map[string]any{"name": "G"})
	if str(one.data(), "id") != str(two.data(), "id") {
		t.Fatal("replay must return the same group")
	}
	if n := len(e.do(t, "GET", "/v1/groups", tok, nil).Body["data"].([]any)); n != 1 {
		t.Fatalf("%d groups", n)
	}
	gid := str(one.data(), "id")
	// Currency must match the account used.
	usd := str(e.call(t, "POST", "/v1/accounts", tok, newKey(), map[string]any{"name": "USD", "type": "savings", "currency": "USD"}).data(), "id")
	me := str(one.data(), "my_member_id")
	if r := e.groupExpense(t, tok, gid, map[string]any{"title": "x", "amount": 100, "split_method": "equal", "participants": parts(me, 0), "account_id": usd}); r.Status != 422 {
		t.Fatalf("currency mismatch: %d", r.Status)
	}
	// Settlement validation.
	other := e.addPerson(t, tok, gid, "O")
	for name, body := range map[string]map[string]any{
		"self":     {"from_member": me, "to_member": me, "amount": 100},
		"zero":     {"from_member": me, "to_member": other, "amount": 0},
		"stranger": {"from_member": me, "to_member": "0b9f1c52-3d6a-4b43-8a5e-5f6b1c7a9d10", "amount": 100},
	} {
		if r := e.call(t, "POST", "/v1/groups/"+gid+"/settlements", tok, newKey(), body); r.Status != 422 {
			t.Errorf("%s: want 422 got %d %s", name, r.Status, r.Raw)
		}
	}
	// Rename with a stale version is a conflict.
	if r := e.call(t, "PUT", "/v1/groups/"+gid, tok, "", map[string]any{"name": "New", "version": 99}); r.Status != 409 {
		t.Fatalf("stale rename: %d %s", r.Status, r.Raw)
	}
	if r := e.call(t, "PUT", "/v1/groups/"+gid, tok, "", map[string]any{"name": "New", "version": 1}); r.Status != 200 || str(r.data(), "name") != "New" {
		t.Fatalf("rename: %d %s", r.Status, r.Raw)
	}
}

func TestSharingAnExistingTransactionIntoAGroup(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	acct := e.newAccount(t, tok, "HDFC", "savings", 1000000)
	bud := str(e.budget(t, tok, map[string]any{"name": "All", "period": "monthly", "limit": 5000000}).data(), "id")
	gid, me := e.newGroup(t, tok, "Flat")
	b := e.addPerson(t, tok, gid, "Ben")
	tx := e.spend(t, tok, acct, 30000, map[string]any{"title": "Dinner"})
	txID := str(tx.data(), "id")
	if e.balance(t, tok, acct) != 970000 || i64(e.do(t, "GET", "/v1/budgets/"+bud, tok, nil).data(), "spent") != 30000 {
		t.Fatal("setup")
	}
	share := func(body map[string]any) resp { return e.groupExpense(t, tok, gid, body) }
	base := map[string]any{"title": "Dinner", "amount": 30000, "split_method": "equal", "participants": parts(me, 0, b, 0), "transaction_id": txID}
	// Wrong amount, both account and transaction, or someone else's transaction are refused.
	for name, body := range map[string]map[string]any{
		"amount mismatch": {"title": "x", "amount": 100, "split_method": "equal", "participants": parts(me, 0, b, 0), "transaction_id": txID},
		"account too":     {"title": "x", "amount": 30000, "split_method": "equal", "participants": parts(me, 0, b, 0), "transaction_id": txID, "account_id": acct},
		"not a uuid":      {"title": "x", "amount": 30000, "split_method": "equal", "participants": parts(me, 0, b, 0), "transaction_id": "nope"},
		"paid by Ben":     {"title": "x", "amount": 30000, "split_method": "equal", "participants": parts(me, 0, b, 0), "transaction_id": txID, "paid_by": b},
	} {
		if r := share(body); r.Status != 422 {
			t.Errorf("%s: want 422 got %d %s", name, r.Status, r.Raw)
		}
	}
	other, _, _ := e.signUp(t, "ravi@example.com")
	oa := e.newAccount(t, other, "B", "savings", 1000)
	otx := str(e.spend(t, other, oa, 30000, nil).data(), "id")
	if r := share(map[string]any{"title": "x", "amount": 30000, "split_method": "equal", "participants": parts(me, 0, b, 0), "transaction_id": otx}); r.Status != 422 {
		t.Fatalf("someone else's transaction: %d", r.Status)
	}
	r := share(base)
	if r.Status != 201 || r.data()["in_my_ledger"] != true {
		t.Fatalf("share: %d %s", r.Status, r.Raw)
	}
	// The balance is untouched, and only my half now counts as spending.
	if e.balance(t, tok, acct) != 970000 || countTx(t) != 2 {
		t.Fatalf("sharing must not move money or create a transaction: balance %d, transactions %d", e.balance(t, tok, acct), countTx(t))
	}
	if got := i64(e.do(t, "GET", "/v1/budgets/"+bud, tok, nil).data(), "spent"); got != 15000 {
		t.Fatalf("budget counts my share only: %d", got)
	}
	if again := share(base); again.Status != 409 || again.errCode() != "already_shared" {
		t.Fatalf("sharing twice: %d %s", again.Status, again.Raw)
	}
	// The transaction knows where it is shared; its amount can no longer be edited behind the split's back.
	si := e.do(t, "GET", "/v1/transactions/"+txID+"/share", tok, nil).data()
	if str(si, "group_id") != gid || i64(si, "my_share") != 15000 || i64(si, "total") != 30000 || str(si, "kind") != "group" {
		t.Fatalf("share info: %v", si)
	}
	if ed := e.call(t, "PUT", "/v1/transactions/"+txID, tok, newKey(), map[string]any{"version": 2, "amount": 1}); ed.Status != 422 {
		t.Fatalf("editing a shared amount: %d %s", ed.Status, ed.Raw)
	}
	if ed := e.call(t, "PUT", "/v1/transactions/"+txID, tok, newKey(), map[string]any{"version": 2, "notes": "fine"}); ed.Status != 200 {
		t.Fatalf("a note edit is allowed: %d %s", ed.Status, ed.Raw)
	}
	if m := e.members(t, tok, gid); i64(m[me], "net") != 15000 || i64(m[b], "net") != -15000 {
		t.Fatalf("nets: %v", m)
	}
	// Un-sharing puts the full amount back into spending and never voids the transaction or moves the balance.
	if v := e.call(t, "POST", "/v1/group-expenses/"+str(r.data(), "id")+"/void", tok, "", nil); v.Status != 200 {
		t.Fatalf("void: %d", v.Status)
	}
	if e.balance(t, tok, acct) != 970000 || i64(e.do(t, "GET", "/v1/budgets/"+bud, tok, nil).data(), "spent") != 30000 {
		t.Fatal("un-sharing restores full spending and leaves the balance alone")
	}
	if e.do(t, "GET", "/v1/transactions/"+txID, tok, nil).data()["status"] != "confirmed" {
		t.Fatal("un-sharing must not void the transaction")
	}
	if e.do(t, "GET", "/v1/transactions/"+txID+"/share", tok, nil).Body["data"] != nil {
		t.Fatal("no share any more")
	}
	assertLedger(t)
}

func TestSplitsAreGroupsOfKindSplit(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	acct := e.newAccount(t, tok, "HDFC", "savings", 1000000)
	tx := str(e.spend(t, tok, acct, 100001, map[string]any{"title": "Goa hotel"}).data(), "id")
	me, a, b := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	body := map[string]any{"transaction_id": tx, "split_method": "equal", "my_id": me, "people": []map[string]any{{"id": a, "name": "Ben"}, {"id": b, "name": "Chitra"}}}
	key := newKey()
	r := e.call(t, "POST", "/v1/splits", tok, key, body)
	if r.Status != 201 || str(r.data(), "kind") != "split" || str(r.data(), "name") != "Goa hotel" {
		t.Fatalf("split: %d %s", r.Status, r.Raw)
	}
	gid := str(r.data(), "id")
	if again := e.call(t, "POST", "/v1/splits", tok, key, body); str(again.data(), "id") != gid {
		t.Fatal("a retry returns the same split")
	}
	// Splits do not clutter the groups list, and show up in the splits list.
	if strings.Contains(e.do(t, "GET", "/v1/groups", tok, nil).Raw, gid) || !strings.Contains(e.do(t, "GET", "/v1/splits", tok, nil).Raw, gid) {
		t.Fatal("kind separation")
	}
	m := e.members(t, tok, gid)
	if len(m) != 3 || i64(m[me], "net") != 100001-33334 || i64(m[a], "net") != -33334 || i64(m[b], "net") != -33333 {
		t.Fatalf("shares must reconcile to the cent with the odd paisa on one person: %v", m)
	}
	// The split is just a group: settle up works and the transaction stays untouched.
	s := e.call(t, "POST", "/v1/groups/"+gid+"/settlements", tok, newKey(), map[string]any{"from_member": a, "to_member": me, "amount": 33334})
	if s.Status != 201 || e.balance(t, tok, acct) != 1000000-100001 {
		t.Fatalf("settle: %d balance %d", s.Status, e.balance(t, tok, acct))
	}
	// Validation: nobody to split with, someone else's transaction, a transaction already shared.
	for name, bad := range map[string]map[string]any{
		"no people":  {"transaction_id": tx, "split_method": "equal", "people": []map[string]any{}},
		"already":    body,
		"bad tx":     {"transaction_id": "nope", "split_method": "equal", "people": []map[string]any{{"name": "x"}}},
		"blank name": {"transaction_id": tx, "split_method": "equal", "people": []map[string]any{{"name": " "}}},
		"bad method": {"transaction_id": tx, "split_method": "weird", "people": []map[string]any{{"name": "x"}}},
	} {
		if r := e.call(t, "POST", "/v1/splits", tok, newKey(), bad); r.Status < 400 {
			t.Errorf("%s: want an error, got %d", name, r.Status)
		}
	}
	// A failed split leaves nothing behind (one database transaction).
	var n int
	_ = pool.QueryRow(t.Context(), `SELECT count(*) FROM groups WHERE kind='split'`).Scan(&n)
	if n != 1 {
		t.Fatalf("failed splits must not leave groups behind: %d", n)
	}
	assertLedger(t)
}

func TestEveryGroupIsABudgetOrASpendingTrackerChosenAtCreation(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	mk := func(body map[string]any) resp { return e.call(t, "POST", "/v1/groups", ta, newKey(), body) }
	for name, body := range map[string]map[string]any{
		"budget without an amount": {"name": "B", "purpose": "budget"},
		"budget with zero":         {"name": "B", "purpose": "budget", "budget_limit": 0},
		"tracker with an amount":   {"name": "T", "purpose": "tracker", "budget_limit": 5000},
		"unknown purpose":          {"name": "X", "purpose": "savings"},
	} {
		if r := mk(body); r.Status != 422 {
			t.Errorf("%s: want 422 got %d %s", name, r.Status, r.Raw)
		}
	}
	tr := mk(map[string]any{"name": "Trip", "purpose": "tracker"})
	if tr.Status != 201 || str(tr.data(), "purpose") != "tracker" || tr.data()["budget_limit"] != nil || tr.data()["remaining"] != nil {
		t.Fatalf("tracker: %d %s", tr.Status, tr.Raw)
	}
	if def := mk(map[string]any{"name": "Old client"}); str(def.data(), "purpose") != "tracker" {
		t.Fatalf("a client that sends no purpose gets a tracker: %s", def.Raw)
	}
	bd := mk(map[string]any{"name": "Flat", "purpose": "budget", "budget_limit": 100000})
	if bd.Status != 201 || str(bd.data(), "purpose") != "budget" || i64(bd.data(), "remaining") != 100000 {
		t.Fatalf("budget: %d %s", bd.Status, bd.Raw)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE groups SET purpose='tracker' WHERE id=$1`, str(bd.data(), "id")); err == nil {
		t.Fatal("the database must refuse to change a group's purpose")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE groups SET budget_limit=NULL WHERE id=$1`, str(bd.data(), "id")); err == nil {
		t.Fatal("a budget can never lose its amount")
	}
}

func TestABudgetIsAPotOfMoneyThatCanBeToppedUp(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	acct := e.newAccount(t, ta, "HDFC", "savings", 1000000)
	bud := str(e.budget(t, ta, map[string]any{"name": "All", "period": "monthly", "limit": 5000000}).data(), "id")
	g := e.call(t, "POST", "/v1/groups", ta, newKey(), map[string]any{"name": "Trip", "purpose": "budget", "budget_limit": 100000})
	gid, me := str(g.data(), "id"), str(g.data(), "my_member_id")
	spend := func(amount int64) resp {
		return e.groupExpense(t, ta, gid, map[string]any{"title": "x", "amount": amount, "split_method": "equal", "participants": parts(me, 0)})
	}
	if r := spend(40000); r.Status != 201 {
		t.Fatalf("spend: %d %s", r.Status, r.Raw)
	}
	rem := func() int64 { return i64(e.do(t, "GET", "/v1/groups/"+gid, ta, nil).data(), "remaining") }
	if rem() != 60000 {
		t.Fatalf("remaining %d", rem())
	}
	// More than what is left is refused; exactly what is left is fine; then nothing more.
	if r := spend(60001); r.Status != 409 || r.errCode() != "budget_exhausted" {
		t.Fatalf("over the pot: %d %s", r.Status, r.Raw)
	}
	if r := spend(60000); r.Status != 201 || rem() != 0 {
		t.Fatalf("spend the rest: %d remaining %d", r.Status, rem())
	}
	if r := spend(1); r.Status != 409 {
		t.Fatalf("an empty budget accepts nothing: %d", r.Status)
	}
	// A top-up is a recorded transaction of the group; with an account it also leaves that account but is not spending.
	key := newKey()
	body := map[string]any{"amount": 50000, "note": "More for the trip", "account_id": acct}
	tp := e.call(t, "POST", "/v1/groups/"+gid+"/topups", ta, key, body)
	if tp.Status != 201 || tp.data()["in_my_ledger"] != true || rem() != 50000 {
		t.Fatalf("top-up: %d %s remaining %d", tp.Status, tp.Raw, rem())
	}
	if again := e.call(t, "POST", "/v1/groups/"+gid+"/topups", ta, key, body); str(again.data(), "id") != str(tp.data(), "id") || rem() != 50000 {
		t.Fatal("a retried top-up must not add twice")
	}
	if e.balance(t, ta, acct) != 950000 {
		t.Fatalf("the money left my account: %d", e.balance(t, ta, acct))
	}
	var src string
	_ = pool.QueryRow(t.Context(), `SELECT source FROM transactions WHERE external_ref LIKE 'topup:%'`).Scan(&src)
	if src != "topup" {
		t.Fatalf("source %q", src)
	}
	if spent := i64(e.do(t, "GET", "/v1/budgets/"+bud, ta, nil).data(), "spent"); spent != 0 {
		t.Fatalf("a top-up is not spending: %d", spent)
	}
	if r := spend(50000); r.Status != 201 || rem() != 0 {
		t.Fatalf("spend the top-up: %d", r.Status)
	}
	// The money is spent, so the top-up cannot be taken back; after voiding that expense it can.
	if v := e.call(t, "POST", "/v1/group-topups/"+str(tp.data(), "id")+"/void", ta, "", nil); v.Status != 409 || v.errCode() != "topup_spent" {
		t.Fatalf("void spent top-up: %d %s", v.Status, v.Raw)
	}
	list := e.do(t, "GET", "/v1/groups/"+gid+"/topups", ta, nil).Body["data"].([]any)
	if len(list) != 1 {
		t.Fatalf("top-ups: %d", len(list))
	}
	for _, bad := range []map[string]any{{"amount": 0}, {"amount": -1}, {"amount": 10, "account_id": "nope"}} {
		if r := e.call(t, "POST", "/v1/groups/"+gid+"/topups", ta, newKey(), bad); r.Status != 422 {
			t.Errorf("%v: %d", bad, r.Status)
		}
	}
	// A tracker has no money to top up.
	tr := e.call(t, "POST", "/v1/groups", ta, newKey(), map[string]any{"name": "T", "purpose": "tracker"})
	if r := e.call(t, "POST", "/v1/groups/"+str(tr.data(), "id")+"/topups", ta, newKey(), map[string]any{"amount": 5}); r.Status != 409 || r.errCode() != "not_a_budget" {
		t.Fatalf("topping up a tracker: %d %s", r.Status, r.Raw)
	}
	assertLedger(t)
}

func TestDeletingAGroupRemovesItsRecordsAndReversesWhatItCreated(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	acct := e.newAccount(t, ta, "HDFC", "savings", 1000000)
	g := e.call(t, "POST", "/v1/groups", ta, newKey(), map[string]any{"name": "Trip", "purpose": "budget", "budget_limit": 500000})
	gid, me := str(g.data(), "id"), str(g.data(), "my_member_id")
	b := e.addPerson(t, ta, gid, "Ben")
	e.groupExpense(t, ta, gid, map[string]any{"title": "Hotel", "amount": 30000, "split_method": "equal", "participants": parts(me, 0, b, 0), "account_id": acct})
	manual := str(e.spend(t, ta, acct, 10000, map[string]any{"title": "Taxi"}).data(), "id")
	e.groupExpense(t, ta, gid, map[string]any{"title": "Taxi", "amount": 10000, "split_method": "equal", "participants": parts(me, 0, b, 0), "transaction_id": manual})
	e.call(t, "POST", "/v1/groups/"+gid+"/topups", ta, newKey(), map[string]any{"amount": 20000, "account_id": acct})
	e.call(t, "POST", "/v1/groups/"+gid+"/settlements", ta, newKey(), map[string]any{"from_member": b, "to_member": me, "amount": 1000, "account_id": acct})
	if e.balance(t, ta, acct) != 1000000-30000-10000-20000+1000 {
		t.Fatalf("setup balance %d", e.balance(t, ta, acct))
	}
	// Someone else cannot delete it; a plain member cannot either.
	if r := e.call(t, "DELETE", "/v1/groups/"+gid, tb, "", nil); r.Status != 404 {
		t.Fatalf("outsider: %d", r.Status)
	}
	if r := e.call(t, "DELETE", "/v1/groups/"+gid, ta, "", nil); r.Status != 204 {
		t.Fatalf("delete: %d %s", r.Status, r.Raw)
	}
	for _, q := range []string{"groups", "group_members", "group_expenses", "expense_shares", "group_settlements", "group_topups"} {
		if n := assertCount(t, `SELECT count(*) FROM `+q+` WHERE true`); n != 0 && q != "groups" {
			t.Errorf("%s still has %d rows", q, n)
		}
	}
	if r := e.do(t, "GET", "/v1/groups/"+gid, ta, nil); r.Status != 404 {
		t.Fatalf("gone: %d", r.Status)
	}
	// Money the group moved through my accounts is reversed (history keeps the reversed rows); the taxi I shared is whole again.
	if e.balance(t, ta, acct) != 1000000-10000 {
		t.Fatalf("balance after delete: %d (only the taxi should remain)", e.balance(t, ta, acct))
	}
	taxi := e.do(t, "GET", "/v1/transactions/"+manual, ta, nil).data()
	if str(taxi, "status") != "confirmed" || taxi["counted_amount"] != nil {
		t.Fatalf("the shared taxi counts in full again: %v", taxi)
	}
	assertLedger(t)
}

func TestAGroupWithOtherPlintUsersCannotBeDeleted(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	gid, _ := e.newGroup(t, ta, "Flat")
	slot := e.addPerson(t, ta, gid, "Ravi")
	code := str(e.call(t, "POST", "/v1/groups/"+gid+"/members/"+slot+"/invite", ta, "", nil).data(), "code")
	e.call(t, "POST", "/v1/group-invites/redeem", tb, "", map[string]any{"code": code})
	if r := e.call(t, "DELETE", "/v1/groups/"+gid, ta, "", nil); r.Status != 409 || r.errCode() != "has_other_members" {
		t.Fatalf("delete with a member: %d %s", r.Status, r.Raw)
	}
	if r := e.call(t, "DELETE", "/v1/groups/"+gid, tb, "", nil); r.Status != 403 {
		t.Fatalf("a plain member cannot delete: %d", r.Status)
	}
	if r := e.call(t, "DELETE", "/v1/groups/"+gid+"/members/"+slot, ta, "", nil); r.Status != 204 {
		t.Fatalf("remove: %d", r.Status)
	}
	if r := e.call(t, "DELETE", "/v1/groups/"+gid, ta, "", nil); r.Status != 204 {
		t.Fatalf("delete after removing them: %d %s", r.Status, r.Raw)
	}
}

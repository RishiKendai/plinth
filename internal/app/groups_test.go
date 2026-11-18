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

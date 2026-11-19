package app_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	e              *env
	tok, hdfc, sbi string
}

// A month with known answers. Every number in the tests below is worked out by hand from this list.
//
//	October 2026 (IST):  income 100,000.00 salary (Oct 10)
//	  Groceries 12,000 (Oct 2) and 8,000 (Oct 20); Food and dining 5,000 (Oct 5); Transport 1,000 (pending, Oct 9)
//	  Shopping 20,000 (Oct 28, UPI) with a 3,000 refund received on Nov 2 (counts against October)
//	  Group dinner 30,000 paid by the user, split 3 ways: only the 10,000 share counts (Oct 15)
//	  Not counted: a 50,000 transfer SBI->HDFC, a cancelled 7,777 expense, a failed 6,666 expense
//	September: Groceries 10,000 (Sep 12)
func monthFixture(t *testing.T) fixture {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	hdfc := e.newAccount(t, tok, "HDFC", "savings", 1000000)
	sbi := e.newAccount(t, tok, "SBI", "savings", 500000)
	cat := func(n string) string { return catID(t, e, tok, n) }
	at := func(d string) string { return d + "T10:00:00+05:30" }
	must := func(r resp) resp {
		t.Helper()
		if r.Status != 201 {
			t.Fatalf("setup: %d %s", r.Status, r.Raw)
		}
		return r
	}
	must(e.call(t, "POST", "/v1/transactions", tok, newKey(), map[string]any{"account_id": hdfc, "type": "income", "amount": 10000000, "title": "Salary", "occurred_at": at("2026-10-10")}))
	must(e.spend(t, tok, hdfc, 1200000, map[string]any{"title": "BigBasket", "category_id": cat("Groceries"), "occurred_at": at("2026-10-02")}))
	must(e.spend(t, tok, hdfc, 800000, map[string]any{"title": "BigBasket", "category_id": cat("Groceries"), "occurred_at": at("2026-10-20")}))
	must(e.spend(t, tok, hdfc, 500000, map[string]any{"title": "Swiggy", "payee_name": "Swiggy", "category_id": cat("Food and dining"), "occurred_at": at("2026-10-05")}))
	must(e.spend(t, tok, hdfc, 100000, map[string]any{"title": "Metro", "category_id": cat("Transport"), "status": "pending", "occurred_at": at("2026-10-09")}))
	shop := must(e.spend(t, tok, hdfc, 2000000, map[string]any{"title": "Phone case", "category_id": cat("Shopping"), "payment_method": "upi", "occurred_at": at("2026-10-28")}))
	must(e.call(t, "POST", "/v1/transactions", tok, newKey(), map[string]any{"account_id": hdfc, "type": "income", "amount": 300000, "title": "Refund", "refund_of": str(shop.data(), "id"), "occurred_at": at("2026-11-02")}))
	must(e.spend(t, tok, hdfc, 7777, map[string]any{"title": "Cancelled", "status": "pending", "occurred_at": at("2026-10-11")}))
	must(e.spend(t, tok, hdfc, 6666, map[string]any{"title": "Failed", "status": "pending", "occurred_at": at("2026-10-12")}))
	must(e.spend(t, tok, hdfc, 1000000, map[string]any{"title": "Phone repair Sept", "category_id": cat("Groceries"), "occurred_at": at("2026-09-12")}))
	// cancel and fail the two throwaway pending ones
	for _, x := range e.do(t, "GET", "/v1/transactions?limit=50", tok, nil).Body["data"].([]any) {
		m := x.(map[string]any)
		switch str(m, "title") {
		case "Cancelled":
			e.call(t, "POST", "/v1/transactions/"+str(m, "id")+"/status", tok, newKey(), map[string]any{"status": "cancelled"})
		case "Failed":
			e.call(t, "POST", "/v1/transactions/"+str(m, "id")+"/status", tok, newKey(), map[string]any{"status": "failed"})
		}
	}
	if r := e.call(t, "POST", "/v1/transfers", tok, newKey(), map[string]any{"from_account_id": sbi, "to_account_id": hdfc, "amount": 5000000, "occurred_at": at("2026-10-14")}); r.Status != 201 {
		t.Fatalf("transfer %d %s", r.Status, r.Raw)
	}
	gid, me := e.newGroup(t, tok, "Flat")
	b, c := e.addPerson(t, tok, gid, "Ben"), e.addPerson(t, tok, gid, "Chitra")
	must(e.groupExpense(t, tok, gid, map[string]any{"title": "Dinner", "amount": 3000000, "split_method": "equal", "participants": parts(me, 0, b, 0, c, 0), "account_id": hdfc, "occurred_at": at("2026-10-15")}))
	return fixture{e, tok, hdfc, sbi}
}

const oct = "from=2026-10-01&to=2026-10-31"

func (f fixture) get(t *testing.T, path string) resp {
	t.Helper()
	r := f.e.do(t, "GET", path, f.tok, nil)
	if r.Status != 200 {
		t.Fatalf("%s: %d %s", path, r.Status, r.Raw)
	}
	return r
}

func TestAnalyticsOverviewUsesOneDefinition(t *testing.T) {
	f := monthFixture(t)
	d := f.get(t, "/v1/analytics/overview?"+oct).data()
	cur := d["current"].(map[string]any)
	// 12000+8000+5000+1000+20000-3000 (refund) +10000 (own share of the group dinner) = 53,000.00
	if i64(cur, "expense") != 5300000 || i64(cur, "income") != 10000000 || i64(cur, "net") != 4700000 {
		t.Fatalf("october: %v", cur)
	}
	if i64(cur, "count") != 7 { // salary, 2 groceries, swiggy, metro, phone case, dinner; the refund is not a transaction here
		t.Fatalf("count %d", i64(cur, "count"))
	}
	if i64(d, "days") != 31 || i64(d, "avg_daily_expense") != 5300000/31 || i64(d, "savings_rate_bp") != 4700 {
		t.Fatalf("derived: %v", d)
	}
	if d["enough_data"] != true {
		t.Fatal("enough data")
	}
	// Previous equal period (Aug 31 - Sep 30) holds only the September groceries.
	pv := d["previous"].(map[string]any)
	if i64(pv, "expense") != 1000000 || i64(pv, "income") != 0 || str(d, "previous_from") != "2026-08-31" || str(d, "previous_to") != "2026-09-30" {
		t.Fatalf("previous: %v", d)
	}
	// An account filter narrows to that account: SBI only moved money by transfer, which is never spending.
	s := f.get(t, "/v1/analytics/overview?"+oct+"&account_id="+f.sbi).data()["current"].(map[string]any)
	if i64(s, "expense") != 0 || i64(s, "income") != 0 {
		t.Fatalf("a transfer must not be income or spending: %v", s)
	}
	// No income: savings rate is null, not zero or a division error.
	n := f.get(t, "/v1/analytics/overview?from=2026-09-01&to=2026-09-30").data()
	if n["savings_rate_bp"] != nil {
		t.Fatalf("savings rate without income: %v", n["savings_rate_bp"])
	}
}

func TestAnalyticsBreakdowns(t *testing.T) {
	f := monthFixture(t)
	rows := func(q string) map[string]int64 {
		out := map[string]int64{}
		var sum int64
		for _, x := range f.get(t, "/v1/analytics/"+q).data()["rows"].([]any) {
			m := x.(map[string]any)
			out[str(m, "label")] = i64(m, "amount")
			sum += i64(m, "amount")
		}
		out["#sum"] = sum
		return out
	}
	c := rows("expenses?" + oct)
	if c["Groceries"] != 2000000 || c["Food and dining"] != 500000 || c["Transport"] != 100000 || c["Shopping"] != 1700000 || c["Uncategorised"] != 1000000 || c["#sum"] != 5300000 {
		t.Fatalf("categories: %v", c)
	}
	if r := rows("income?" + oct); r["Uncategorised"] != 10000000 || len(r) != 2 {
		t.Fatalf("income: %v", r)
	}
	pm := rows("expenses?" + oct + "&by=payment_method")
	if pm["UPI"] != 0 && pm["upi"] != 1700000 && pm["#sum"] != 5300000 {
		t.Fatalf("payment methods: %v", pm)
	}
	if pm["#sum"] != 5300000 {
		t.Fatalf("payment method rows must add up to the total: %v", pm)
	}
	if m := rows("expenses?" + oct + "&by=merchant"); m["#sum"] != 5300000 || m["Swiggy"] != 500000 {
		t.Fatalf("merchants: %v", m)
	}
	if a := rows("accounts?" + oct); a["HDFC"] != 5300000 || a["#sum"] != 5300000 {
		t.Fatalf("accounts: %v", a)
	}
	// Percent shares come from the listed total and are basis points.
	for _, x := range f.get(t, "/v1/analytics/expenses?"+oct).data()["rows"].([]any) {
		m := x.(map[string]any)
		if str(m, "label") == "Groceries" && i64(m, "percent_bp") != 2000000*10000/5300000 {
			t.Fatalf("share %v", m)
		}
	}
	// A limit folds the tail into "Other" so rows still add up.
	l := rows("expenses?" + oct + "&limit=2")
	if l["#sum"] != 5300000 || l["Other"] == 0 || len(l) != 3 {
		t.Fatalf("limit: %v", l)
	}
	if r := f.e.do(t, "GET", "/v1/analytics/expenses?by=nonsense", f.tok, nil); r.Status != 422 {
		t.Fatalf("bad grouping: %d", r.Status)
	}
}

func TestAnalyticsTagsCountEachTag(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 100000)
	e.spend(t, tok, a, 1000, map[string]any{"tags": []string{"trip", "food"}, "occurred_at": "2026-10-03T10:00:00Z"})
	e.spend(t, tok, a, 500, map[string]any{"tags": []string{"trip"}, "occurred_at": "2026-10-04T10:00:00Z"})
	got := map[string]int64{}
	for _, x := range e.do(t, "GET", "/v1/analytics/expenses?by=tag&"+oct, tok, nil).data()["rows"].([]any) {
		m := x.(map[string]any)
		got[str(m, "label")] = i64(m, "amount")
	}
	if got["trip"] != 1500 || got["food"] != 1000 {
		t.Fatalf("tags: %v", got)
	}
}

func TestAnalyticsTrendsHeatmapCompare(t *testing.T) {
	f := monthFixture(t)
	pts := f.get(t, "/v1/analytics/trends?"+oct+"&granularity=day").data()["points"].([]any)
	if len(pts) != 31 {
		t.Fatalf("every day is present, even empty ones: %d", len(pts))
	}
	var exp, inc int64
	for _, p := range pts {
		exp += i64(p.(map[string]any), "expense")
		inc += i64(p.(map[string]any), "income")
	}
	if exp != 5300000 || inc != 10000000 {
		t.Fatalf("daily buckets must add up to the totals: %d %d", exp, inc)
	}
	if last := pts[30].(map[string]any); i64(last, "cumulative_net") != 4700000 || str(last, "date") != "2026-10-31" {
		t.Fatalf("cumulative: %v", last)
	}
	oct2 := pts[1].(map[string]any) // Oct 2: the 12,000 groceries
	if i64(oct2, "expense") != 1200000 {
		t.Fatalf("oct 2: %v", oct2)
	}
	// Monthly over three months: the refund lands in October (the expense's month), not November.
	m := f.get(t, "/v1/analytics/trends?from=2026-09-01&to=2026-11-30&granularity=month").data()["points"].([]any)
	if len(m) != 3 || i64(m[0].(map[string]any), "expense") != 1000000 || i64(m[1].(map[string]any), "expense") != 5300000 || i64(m[2].(map[string]any), "expense") != 0 {
		t.Fatalf("months: %v", m)
	}
	w := f.get(t, "/v1/analytics/trends?"+oct+"&granularity=week").data()["points"].([]any)
	if str(w[0].(map[string]any), "date") != "2026-09-28" { // Oct 1 2026 is a Thursday; its week starts Monday Sep 28
		t.Fatalf("weeks start on Monday: %v", w[0])
	}
	h := f.get(t, "/v1/analytics/heatmap?"+oct).data()
	var ws int64
	for _, v := range h["weekday_total"].([]any) {
		ws += int64(v.(float64))
	}
	if i64(h, "max") != 2000000-300000+0 && i64(h, "max") != 2000000 || ws != 5300000 {
		t.Fatalf("heatmap: max %d weekday sum %d", i64(h, "max"), ws)
	}
	cmp := f.get(t, "/v1/analytics/compare?"+oct).data()["rows"].([]any)
	var found bool
	for _, x := range cmp {
		r := x.(map[string]any)
		if str(r, "label") == "Groceries" {
			found = i64(r, "current") == 2000000 && i64(r, "previous") == 1000000 && i64(r, "change") == 1000000
		}
	}
	if !found {
		t.Fatalf("compare: %v", cmp)
	}
	if r := f.e.do(t, "GET", "/v1/analytics/trends?granularity=hour", f.tok, nil); r.Status != 422 {
		t.Fatalf("granularity: %d", r.Status)
	}
}

func TestAnalyticsActualAndExpectedStaySeparate(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 5000000)
	start := todayIST().AddDate(0, 0, 3)
	if r := e.schedule(t, tok, rentSchedule(a, start)); r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	to := start.AddDate(0, 0, 20).Format("2006-01-02")
	d := e.do(t, "GET", "/v1/analytics/overview?from="+start.Format("2006-01-02")+"&to="+to, tok, nil).data()
	if i64(d["current"].(map[string]any), "expense") != 0 {
		t.Fatalf("a schedule is not spending: %v", d["current"])
	}
	if i64(d["expected"].(map[string]any), "expense") <= 0 {
		t.Fatalf("expected rent missing: %v", d["expected"])
	}
}

func TestAnalyticsInsightsAreDescriptive(t *testing.T) {
	f := monthFixture(t)
	d := f.get(t, "/v1/insights?"+oct).data()
	list := d["insights"].([]any)
	if len(list) < 3 {
		t.Fatalf("insights: %v", list)
	}
	ids := map[string]bool{}
	for _, x := range list {
		m := x.(map[string]any)
		ids[str(m, "id")] = true
		txt := strings.ToLower(str(m, "text"))
		for _, bad := range []string{"should", "recommend", "advice", "you must", "consider", "try to", "invest", "!"} {
			if strings.Contains(txt, bad) {
				t.Fatalf("insights describe the past and give no advice: %q", txt)
			}
		}
	}
	if !ids["top-category"] || !ids["largest-expense"] || !ids["net"] {
		t.Fatalf("expected insights missing: %v", ids)
	}
	// Not enough data: an empty list, never invented text.
	e := setup(t)
	tok, _, _ := e.signUp(t, "new@example.com")
	if r := e.do(t, "GET", "/v1/insights", tok, nil); len(r.data()["insights"].([]any)) != 0 {
		t.Fatalf("no data, no insights: %s", r.Raw)
	}
}

func TestAnalyticsAccountLedgerView(t *testing.T) {
	f := monthFixture(t)
	d := f.get(t, "/v1/accounts/"+f.hdfc+"/analytics?"+oct).data()
	// Opening 10,000.00 less the Sept groceries 10,000.00 = 0 at Oct 1.
	if i64(d, "opening_balance") != 0 {
		t.Fatalf("opening at Oct 1: %v", d["opening_balance"])
	}
	// Money in: salary 100,000 + transfer 50,000 (the Nov 2 refund is outside the range). Money out: everything spent.
	if i64(d, "inflow") != 15000000 || i64(d, "outflow") != 1200000+800000+500000+100000+2000000+3000000 {
		t.Fatalf("flows: %v / %v", d["inflow"], d["outflow"])
	}
	h := d["history"].([]any)
	if len(h) != 31 || i64(h[30].(map[string]any), "balance") != i64(d, "closing_balance") {
		t.Fatalf("history: %d points", len(h))
	}
	// Closing = opening + net; and the live balance adds the refund that arrived in November.
	if i64(d, "closing_balance") != i64(d, "opening_balance")+i64(d, "inflow")-i64(d, "outflow") {
		t.Fatal("closing balance must equal opening plus movements")
	}
	if f.e.balance(t, f.tok, f.hdfc) != i64(d, "closing_balance")+300000 {
		t.Fatalf("history must reconcile with the live balance: %d vs %d", f.e.balance(t, f.tok, f.hdfc), i64(d, "closing_balance"))
	}
	assertLedger(t)
}

func TestAnalyticsIsolationAndValidation(t *testing.T) {
	f := monthFixture(t)
	other, _, _ := f.e.signUp(t, "ravi@example.com")
	o := f.e.do(t, "GET", "/v1/analytics/overview?"+oct, other, nil).data()
	if i64(o["current"].(map[string]any), "expense") != 0 || i64(o["current"].(map[string]any), "income") != 0 {
		t.Fatalf("another user's data leaked: %v", o)
	}
	if r := f.e.do(t, "GET", "/v1/analytics/overview?account_id="+f.hdfc, other, nil); r.Status != 404 {
		t.Fatalf("another user's account: %d", r.Status)
	}
	if r := f.e.do(t, "GET", "/v1/accounts/"+f.hdfc+"/analytics", other, nil); r.Status != 404 {
		t.Fatalf("account analytics of another user: %d", r.Status)
	}
	if r := f.e.do(t, "GET", "/v1/analytics/overview", "", nil); r.Status != 401 {
		t.Fatalf("unauthenticated: %d", r.Status)
	}
	for _, q := range []string{"from=nope", "from=2026-10-31&to=2026-10-01", "from=2000-01-01&to=2026-10-01", "account_id=nope"} {
		if r := f.e.do(t, "GET", "/v1/analytics/overview?"+q, f.tok, nil); r.Status != 422 {
			t.Errorf("%s: want 422 got %d", q, r.Status)
		}
	}
	// Different currencies are never added together.
	usd := str(f.e.call(t, "POST", "/v1/accounts", f.tok, newKey(), map[string]any{"name": "USD", "type": "savings", "currency": "USD", "opening_balance": 1000}).data(), "id")
	f.e.spend(t, f.tok, usd, 5000, map[string]any{"occurred_at": "2026-10-05T10:00:00Z"})
	if i64(f.get(t, "/v1/analytics/overview?"+oct).data()["current"].(map[string]any), "expense") != 5300000 {
		t.Fatal("a USD expense leaked into the INR total")
	}
	if i64(f.get(t, "/v1/analytics/overview?"+oct+"&currency=USD").data()["current"].(map[string]any), "expense") != 5000 {
		t.Fatal("USD view")
	}
}

func waitReport(t *testing.T, e *env, tok, id string) map[string]any {
	t.Helper()
	for i := 0; i < 100; i++ {
		d := e.do(t, "GET", "/v1/reports/"+id, tok, nil).data()
		if s := str(d, "status"); s == "ready" || s == "failed" {
			return d
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("report never finished")
	return nil
}

func TestReportsAreGeneratedInTheBackground(t *testing.T) {
	f := monthFixture(t)
	key := newKey()
	body := map[string]any{"kind": "statement", "account_id": f.hdfc, "from": "2026-10-01", "to": "2026-10-31"}
	r := f.e.call(t, "POST", "/v1/reports", f.tok, key, body)
	if r.Status != 202 || str(r.data(), "status") != "queued" {
		t.Fatalf("create: %d %s", r.Status, r.Raw)
	}
	id := str(r.data(), "id")
	if again := f.e.call(t, "POST", "/v1/reports", f.tok, key, body); str(again.data(), "id") != id {
		t.Fatal("a retry returns the same report")
	}
	rep := waitReport(t, f.e, f.tok, id)
	if str(rep, "status") != "ready" {
		t.Fatalf("report: %v", rep)
	}
	dl := f.e.call(t, "GET", "/v1/reports/"+id+"/download", f.tok, "", nil)
	if dl.Status != 200 || !strings.HasPrefix(dl.Raw, "Date,Description,Category,Status,Money out,Money in,Balance") {
		t.Fatalf("download: %d %.200s", dl.Status, dl.Raw)
	}
	// The statement's closing balance is the ledger's, so it reconciles with the account.
	lines := strings.Split(strings.TrimSpace(dl.Raw), "\n")
	closing := lines[len(lines)-1]
	want := fmt.Sprintf("%d.%02d", (f.e.balance(t, f.tok, f.hdfc)-300000)/100, (f.e.balance(t, f.tok, f.hdfc)-300000)%100)
	if !strings.HasSuffix(closing, ","+want) || !strings.Contains(closing, "Closing balance") {
		t.Fatalf("closing line %q want balance %s", closing, want)
	}
	if !strings.Contains(dl.Raw, "Opening balance") || !strings.Contains(dl.Raw, "Salary") {
		t.Fatalf("statement content:\n%s", dl.Raw)
	}
	// Reports are private: another user gets 404, and unauthenticated gets 401.
	other, _, _ := f.e.signUp(t, "ravi@example.com")
	for _, p := range []string{"/v1/reports/" + id, "/v1/reports/" + id + "/download"} {
		if x := f.e.do(t, "GET", p, other, nil); x.Status != 404 {
			t.Fatalf("%s by another user: %d", p, x.Status)
		}
	}
	if strings.Contains(f.e.do(t, "GET", "/v1/reports", other, nil).Raw, id) {
		t.Fatal("report list leaked")
	}
	// Someone else cannot request a statement for my account.
	if x := f.e.call(t, "POST", "/v1/reports", other, newKey(), body); x.Status != 404 {
		t.Fatalf("statement of another user's account: %d", x.Status)
	}
	// Year in review.
	yr := f.e.call(t, "POST", "/v1/reports", f.tok, newKey(), map[string]any{"kind": "year_in_review", "year": 2026})
	yid := str(yr.data(), "id")
	if str(waitReport(t, f.e, f.tok, yid), "status") != "ready" {
		t.Fatal("year in review")
	}
	y := f.e.call(t, "GET", "/v1/reports/"+yid+"/download", f.tok, "", nil)
	if !strings.Contains(y.Raw, "2026-10,100000.00,53000.00,47000.00") || !strings.Contains(y.Raw, "Groceries,30000.00") {
		t.Fatalf("year in review:\n%s", y.Raw)
	}
	for _, bad := range []map[string]any{{"kind": "x"}, {"kind": "statement"}, {"kind": "year_in_review", "year": 1990}} {
		if x := f.e.call(t, "POST", "/v1/reports", f.tok, newKey(), bad); x.Status != 422 {
			t.Errorf("%v: want 422 got %d", bad, x.Status)
		}
	}
}

func TestGroupAnalytics(t *testing.T) {
	f := monthFixture(t)
	var gid string
	for _, g := range f.e.do(t, "GET", "/v1/groups", f.tok, nil).Body["data"].([]any) {
		gid = str(g.(map[string]any), "id")
	}
	d := f.get(t, "/v1/groups/"+gid+"/analytics").data()
	if i64(d, "total_expenses") != 3000000 || i64(d, "expense_count") != 1 || i64(d, "outstanding") != 2000000 {
		t.Fatalf("group: %v", d)
	}
	var paid, owed int64
	for _, m := range d["members"].([]any) {
		paid += i64(m.(map[string]any), "paid")
		owed += i64(m.(map[string]any), "owed")
	}
	if paid != 3000000 || owed != 3000000 {
		t.Fatalf("contributions and obligations each add up to the total: %d %d", paid, owed)
	}
	if mo := d["months"].([]any); len(mo) != 1 || str(mo[0].(map[string]any), "month") != "2026-10" {
		t.Fatalf("months: %v", mo)
	}
	other, _, _ := f.e.signUp(t, "ravi@example.com")
	if r := f.e.do(t, "GET", "/v1/groups/"+gid+"/analytics", other, nil); r.Status != 404 {
		t.Fatalf("non-member: %d", r.Status)
	}
}

func TestReportsCanBeDeletedAndAreCapped(t *testing.T) {
	f := monthFixture(t)
	mk := func() resp {
		return f.e.call(t, "POST", "/v1/reports", f.tok, newKey(), map[string]any{"kind": "statement", "account_id": f.hdfc, "from": "2026-10-01", "to": "2026-10-31"})
	}
	first := mk()
	id := str(first.data(), "id")
	waitReport(t, f.e, f.tok, id)
	other, _, _ := f.e.signUp(t, "ravi@example.com")
	if r := f.e.call(t, "DELETE", "/v1/reports/"+id, other, "", nil); r.Status != 404 {
		t.Fatalf("deleting another user's report: %d", r.Status)
	}
	if r := f.e.call(t, "DELETE", "/v1/reports/"+id, f.tok, "", nil); r.Status != 204 {
		t.Fatalf("delete: %d", r.Status)
	}
	if r := f.e.do(t, "GET", "/v1/reports/"+id, f.tok, nil); r.Status != 404 {
		t.Fatalf("a deleted report is gone: %d", r.Status)
	}
	if assertCount(t, `SELECT count(*) FROM transactions`) == 0 {
		t.Fatal("deleting a report must never touch the ledger")
	}
	// Stuck work is capped: with three reports queued, a fourth is refused.
	if _, err := pool.Exec(context.Background(), `INSERT INTO reports (id, user_id, kind, period_from, period_to, status)
		SELECT gen_random_uuid(), u.id, 'year_in_review', DATE '2026-01-01', DATE '2026-12-31', s.st
		FROM users u, (VALUES ('running'), ('queued'), ('queued')) AS s(st) WHERE u.email = 'asha@example.com'`); err != nil {
		t.Fatal(err)
	}
	if r := mk(); r.Status != 429 || r.errCode() != "too_many_reports" {
		t.Fatalf("cap: %d %s", r.Status, r.Raw)
	}
}

func assertCount(t *testing.T, q string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

package app_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"plinth/server/internal/calendar"
	"plinth/server/internal/recurrence"
)

var ist, _ = time.LoadLocation("Asia/Kolkata")

func todayIST() time.Time { return calendar.Today(time.Now(), ist) }

func dateStr(t time.Time) string { return calendar.Format(t) }

func (e *env) schedule(t *testing.T, tok string, body map[string]any) resp {
	t.Helper()
	return e.call(t, "POST", "/v1/recurrences", tok, newKey(), body)
}

func rentSchedule(acct string, start time.Time) map[string]any {
	return map[string]any{"account_id": acct, "type": "expense", "title": "Rent", "amount": 2800000, "frequency": "monthly", "interval": 1, "start_date": dateStr(start)}
}

func (e *env) occurrences(t *testing.T, tok, id, state string) []map[string]any {
	t.Helper()
	r := e.do(t, "GET", "/v1/recurrences/"+id+"/occurrences?state="+state+"&limit=200", tok, nil)
	if r.Status != 200 {
		t.Fatalf("occurrences: %d %s", r.Status, r.Raw)
	}
	var out []map[string]any
	for _, x := range r.Body["data"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func countOcc(t *testing.T, scheduleID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recurring_occurrences WHERE schedule_id=$1`, scheduleID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRecurringOccurrencesAreExpectationsNotMoney(t *testing.T) { // phase 3 exit: recurrence never moves a balance
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 5000000)
	start := todayIST().AddDate(0, 0, -70)
	r := e.schedule(t, tok, rentSchedule(a, start))
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	id := str(r.data(), "id")
	occ := e.occurrences(t, tok, id, "")
	if len(occ) < 3 {
		t.Fatalf("expected a backlog plus upcoming occurrences, got %d", len(occ))
	}
	if e.balance(t, tok, a) != 5000000 {
		t.Fatal("generating occurrences must not move money")
	}
	var txCount int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM transactions`).Scan(&txCount)
	if txCount != 0 {
		t.Fatalf("occurrences must not create transactions, found %d", txCount)
	}
	// The dates are exactly what the calendar rules produce, anchored on the start day.
	want := calendar.Between(start, calendar.Monthly, 1, start.AddDate(0, 0, -1), todayIST().AddDate(0, 0, recurrence.HorizonDays), nil, 400)
	if len(occ) != len(want) {
		t.Fatalf("got %d occurrences want %d", len(occ), len(want))
	}
	got := map[string]bool{}
	for _, o := range occ {
		got[str(o, "due_date")] = true
	}
	for _, d := range want {
		if !got[dateStr(d)] {
			t.Fatalf("missing occurrence %s", dateStr(d))
		}
	}
	assertLedger(t)
}

func TestOccurrenceGenerationIsIdempotentAndConcurrencySafe(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 0)
	id := str(e.schedule(t, tok, map[string]any{"account_id": a, "type": "expense", "title": "Gym", "amount": 99900, "frequency": "weekly", "interval": 1,
		"start_date": dateStr(todayIST().AddDate(0, 0, -30))}).data(), "id")
	before := countOcc(t, id)
	svc := recurrence.NewService(pool, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.GenerateAll(context.Background(), 10); err != nil {
				t.Errorf("generate: %v", err)
			}
		}()
	}
	wg.Wait()
	if after := countOcc(t, id); after != before {
		t.Fatalf("repeat generation changed the count: %d -> %d", before, after)
	}
	// Force the cursor back: a re-run must refill nothing new because of the unique (schedule, date) index.
	if _, err := pool.Exec(context.Background(), `UPDATE recurring_schedules SET generated_through = start_date - 1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GenerateAll(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if after := countOcc(t, id); after != before {
		t.Fatalf("a rewound cursor created duplicates: %d -> %d", before, after)
	}
	var dups int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM (SELECT schedule_id, due_date FROM recurring_occurrences GROUP BY 1,2 HAVING count(*) > 1) x`).Scan(&dups)
	if dups != 0 {
		t.Fatal("duplicate occurrences")
	}
}

func TestConfirmOccurrenceCreatesExactlyOneTransaction(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 5000000)
	id := str(e.schedule(t, tok, rentSchedule(a, todayIST().AddDate(0, 0, -40))).data(), "id")
	occ := e.occurrences(t, tok, id, "overdue")
	if len(occ) == 0 {
		t.Fatal("expected overdue occurrences")
	}
	oid := str(occ[len(occ)-1], "id")
	path := "/v1/recurrences/" + id + "/occurrences/" + oid + "/confirm"
	key := newKey()
	r := e.call(t, "POST", path, tok, key, map[string]any{})
	if r.Status != 200 || str(r.data(), "status") != "confirmed" || r.data()["transaction_id"] == nil {
		t.Fatalf("confirm: %d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != 5000000-2800000 {
		t.Fatalf("confirming moves money exactly once, balance %d", e.balance(t, tok, a))
	}
	tx := e.do(t, "GET", "/v1/transactions/"+str(r.data(), "transaction_id"), tok, nil).data()
	if str(tx, "source") != "recurring" || str(tx, "status") != "confirmed" || str(tx, "title") != "Rent" {
		t.Fatalf("transaction: %v", tx)
	}
	// Same key replays; a new key finds the occurrence closed; nothing moves again.
	if rr := e.call(t, "POST", path, tok, key, map[string]any{}); rr.Status != 200 || str(rr.data(), "transaction_id") != str(r.data(), "transaction_id") {
		t.Fatalf("replay: %d %s", rr.Status, rr.Raw)
	}
	if rr := e.call(t, "POST", path, tok, newKey(), map[string]any{}); rr.Status != 409 || rr.errCode() != "occurrence_closed" {
		t.Fatalf("second confirm: %d %s", rr.Status, rr.Raw)
	}
	if e.balance(t, tok, a) != 5000000-2800000 {
		t.Fatal("retries moved money again")
	}
	assertLedger(t)
}

func TestConcurrentConfirmCreatesOneTransaction(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 1000000)
	id := str(e.schedule(t, tok, rentSchedule(a, todayIST().AddDate(0, 0, -5))).data(), "id")
	oid := str(e.occurrences(t, tok, id, "")[0], "id")
	var wg sync.WaitGroup
	var ok, closed int
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.call(t, "POST", "/v1/recurrences/"+id+"/occurrences/"+oid+"/confirm", tok, newKey(), map[string]any{"amount": 100000})
			mu.Lock()
			defer mu.Unlock()
			switch r.Status {
			case 200:
				ok++
			case 409:
				closed++
			default:
				t.Errorf("unexpected %d %s", r.Status, r.Raw)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || closed != 7 {
		t.Fatalf("exactly one confirm may win: ok=%d closed=%d", ok, closed)
	}
	if e.balance(t, tok, a) != 900000 {
		t.Fatalf("balance %d", e.balance(t, tok, a))
	}
	assertLedger(t)
}

func TestConfirmWithDifferentAmountShowsExpectedVsActual(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 1000000)
	id := str(e.schedule(t, tok, map[string]any{"account_id": a, "type": "expense", "title": "Electricity", "amount": 150000, "frequency": "monthly",
		"start_date": dateStr(todayIST().AddDate(0, 0, -3))}).data(), "id")
	oid := str(e.occurrences(t, tok, id, "")[0], "id")
	r := e.call(t, "POST", "/v1/recurrences/"+id+"/occurrences/"+oid+"/confirm", tok, newKey(), map[string]any{"amount": 172500})
	if r.Status != 200 || i64(r.data(), "expected_amount") != 150000 || i64(r.data(), "actual_amount") != 172500 || i64(r.data(), "difference") != 22500 {
		t.Fatalf("expected vs actual: %d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != 1000000-172500 {
		t.Fatal("the balance follows what was actually paid")
	}
}

func TestSkipRecordsNothing(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 100000)
	id := str(e.schedule(t, tok, rentSchedule(a, todayIST().AddDate(0, 0, -3))).data(), "id")
	oid := str(e.occurrences(t, tok, id, "overdue")[0], "id")
	r := e.call(t, "POST", "/v1/recurrences/"+id+"/occurrences/"+oid+"/skip", tok, "", nil)
	if r.Status != 200 || str(r.data(), "status") != "skipped" || str(r.data(), "state") != "skipped" {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != 100000 {
		t.Fatal("skipping must not move money")
	}
	if rr := e.call(t, "POST", "/v1/recurrences/"+id+"/occurrences/"+oid+"/confirm", tok, newKey(), map[string]any{}); rr.Status != 409 {
		t.Fatalf("a skipped occurrence cannot be confirmed: %d", rr.Status)
	}
	if n := len(e.do(t, "GET", "/v1/occurrences?state=overdue", tok, nil).Body["data"].([]any)); n != 0 {
		t.Fatalf("a skipped occurrence is no longer overdue, got %d", n)
	}
}

func TestPauseResumeCancel(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 0)
	id := str(e.schedule(t, tok, map[string]any{"account_id": a, "type": "income", "title": "Salary", "amount": 9000000, "frequency": "monthly",
		"start_date": dateStr(todayIST().AddDate(0, 0, 3))}).data(), "id")
	if n := len(e.occurrences(t, tok, id, "upcoming")); n == 0 {
		t.Fatal("expected upcoming occurrences")
	}
	p := e.call(t, "POST", "/v1/recurrences/"+id+"/pause", tok, "", nil)
	if p.Status != 200 || str(p.data(), "status") != "paused" {
		t.Fatalf("pause: %d %s", p.Status, p.Raw)
	}
	if n := len(e.occurrences(t, tok, id, "upcoming")); n != 0 {
		t.Fatalf("pausing drops future expectations, %d left", n)
	}
	// The worker must not regenerate while paused.
	if _, err := recurrence.NewService(pool, nil, nil).GenerateAll(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if n := len(e.occurrences(t, tok, id, "upcoming")); n != 0 {
		t.Fatal("a paused schedule generated occurrences")
	}
	if again := e.call(t, "POST", "/v1/recurrences/"+id+"/pause", tok, "", nil); again.Status != 409 {
		t.Fatalf("pausing twice: %d", again.Status)
	}
	if r := e.call(t, "POST", "/v1/recurrences/"+id+"/resume", tok, "", nil); r.Status != 200 || str(r.data(), "status") != "active" {
		t.Fatalf("resume: %d %s", r.Status, r.Raw)
	}
	if n := len(e.occurrences(t, tok, id, "upcoming")); n == 0 {
		t.Fatal("resuming regenerates future expectations")
	}
	if c := e.call(t, "POST", "/v1/recurrences/"+id+"/cancel", tok, "", nil); c.Status != 200 || str(c.data(), "status") != "cancelled" {
		t.Fatalf("cancel: %d %s", c.Status, c.Raw)
	}
	if n := len(e.occurrences(t, tok, id, "upcoming")); n != 0 {
		t.Fatal("cancelling drops future expectations")
	}
	if r := e.call(t, "POST", "/v1/recurrences/"+id+"/resume", tok, "", nil); r.Status != 409 {
		t.Fatalf("a cancelled schedule cannot resume: %d", r.Status)
	}
	if e.balance(t, tok, a) != 0 {
		t.Fatal("no operation here may move money")
	}
}

func TestEditAppliesToFutureOccurrencesOnly(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 0)
	sched := e.schedule(t, tok, rentSchedule(a, todayIST().AddDate(0, 0, -45)))
	id, ver := str(sched.data(), "id"), int(i64(sched.data(), "version"))
	overdue := e.occurrences(t, tok, id, "overdue")
	upcoming := e.occurrences(t, tok, id, "upcoming")
	if len(overdue) == 0 || len(upcoming) == 0 {
		t.Fatal("need both overdue and upcoming occurrences")
	}
	u := e.do(t, "PUT", "/v1/recurrences/"+id, tok, map[string]any{"version": ver, "amount": 3000000, "title": "Rent (new lease)"})
	if u.Status != 200 {
		t.Fatalf("%d %s", u.Status, u.Raw)
	}
	for _, o := range e.occurrences(t, tok, id, "overdue") {
		if i64(o, "expected_amount") != 2800000 {
			t.Fatalf("an overdue occurrence must keep its original amount, got %d", i64(o, "expected_amount"))
		}
	}
	for _, o := range e.occurrences(t, tok, id, "upcoming") {
		if i64(o, "expected_amount") != 3000000 {
			t.Fatalf("future occurrences take the new amount, got %d", i64(o, "expected_amount"))
		}
	}
	// A pattern change regenerates the future only.
	ver = int(i64(u.data(), "version"))
	pastBefore := len(e.occurrences(t, tok, id, "overdue"))
	p := e.do(t, "PUT", "/v1/recurrences/"+id, tok, map[string]any{"version": ver, "frequency": "weekly"})
	if p.Status != 200 {
		t.Fatalf("%d %s", p.Status, p.Raw)
	}
	if got := len(e.occurrences(t, tok, id, "overdue")); got != pastBefore {
		t.Fatalf("past occurrences must not change: %d -> %d", pastBefore, got)
	}
	up := e.occurrences(t, tok, id, "upcoming")
	if len(up) < 8 { // ~60 days of weekly occurrences
		t.Fatalf("weekly pattern should give about 8 upcoming, got %d", len(up))
	}
	if stale := e.do(t, "PUT", "/v1/recurrences/"+id, tok, map[string]any{"version": ver, "title": "x"}); stale.Status != 409 {
		t.Fatalf("stale version: %d", stale.Status)
	}
	if bad := e.do(t, "PUT", "/v1/recurrences/"+id, tok, map[string]any{"version": ver + 1, "start_date": "2020-01-01"}); bad.Status != 400 {
		t.Fatalf("start date is not editable: %d", bad.Status)
	}
}

func TestRecurrenceValidationAndOwnership(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	a := e.newAccount(t, ta, "A", "savings", 0)
	b := e.newAccount(t, tb, "B", "savings", 0)
	today := todayIST()
	for name, body := range map[string]map[string]any{
		"no title":      {"account_id": a, "type": "expense", "title": "", "amount": 5, "frequency": "monthly", "start_date": dateStr(today)},
		"zero amount":   {"account_id": a, "type": "expense", "title": "x", "amount": 0, "frequency": "monthly", "start_date": dateStr(today)},
		"bad frequency": {"account_id": a, "type": "expense", "title": "x", "amount": 5, "frequency": "fortnightly", "start_date": dateStr(today)},
		"bad date":      {"account_id": a, "type": "expense", "title": "x", "amount": 5, "frequency": "monthly", "start_date": "2026-02-30"},
		"end < start":   {"account_id": a, "type": "expense", "title": "x", "amount": 5, "frequency": "monthly", "start_date": dateStr(today), "end_date": dateStr(today.AddDate(0, 0, -1))},
		"too old":       {"account_id": a, "type": "expense", "title": "x", "amount": 5, "frequency": "monthly", "start_date": dateStr(today.AddDate(-2, 0, 0))},
		"foreign acct":  {"account_id": b, "type": "expense", "title": "x", "amount": 5, "frequency": "monthly", "start_date": dateStr(today)},
		"bad interval":  {"account_id": a, "type": "expense", "title": "x", "amount": 5, "frequency": "monthly", "interval": 400, "start_date": dateStr(today)},
		"bad tz":        {"account_id": a, "type": "expense", "title": "x", "amount": 5, "frequency": "monthly", "start_date": dateStr(today), "timezone": "Mars/Base"},
	} {
		if r := e.schedule(t, ta, body); r.Status != 422 {
			t.Errorf("%s: want 422 got %d %s", name, r.Status, r.Raw)
		}
	}
	id := str(e.schedule(t, ta, rentSchedule(a, today)).data(), "id")
	oid := str(e.occurrences(t, ta, id, "")[0], "id")
	for _, c := range []struct{ m, p, k string }{
		{"GET", "/v1/recurrences/" + id, ""}, {"GET", "/v1/recurrences/" + id + "/occurrences", ""},
		{"POST", "/v1/recurrences/" + id + "/pause", ""}, {"POST", "/v1/recurrences/" + id + "/cancel", ""},
		{"POST", "/v1/recurrences/" + id + "/occurrences/" + oid + "/confirm", newKey()},
		{"POST", "/v1/recurrences/" + id + "/occurrences/" + oid + "/skip", ""},
	} {
		if r := e.call(t, c.m, c.p, tb, c.k, map[string]any{}); r.Status != 404 {
			t.Errorf("%s %s by another user: want 404 got %d", c.m, c.p, r.Status)
		}
	}
	if strings.Contains(e.do(t, "GET", "/v1/recurrences", tb, nil).Raw, id) || strings.Contains(e.do(t, "GET", "/v1/occurrences", tb, nil).Raw, oid) {
		t.Fatal("another user's schedules leaked")
	}
}

func TestSchedulesAreEvaluatedInTheirOwnTimeZone(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 0)
	r := e.schedule(t, tok, map[string]any{"account_id": a, "type": "expense", "title": "Netflix", "amount": 64900, "frequency": "monthly",
		"start_date": dateStr(todayIST().AddDate(0, 0, 1)), "timezone": "America/New_York", "time_of_day": "08:30"})
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	o := e.occurrences(t, tok, str(r.data(), "id"), "upcoming")[0]
	due, _ := time.Parse(time.RFC3339, str(o, "due_at"))
	ny, _ := time.LoadLocation("America/New_York")
	local := due.In(ny)
	if local.Format("15:04") != "08:30" || dateStr(calendar.Date(local.Year(), local.Month(), local.Day())) != str(o, "due_date") {
		t.Fatalf("due_at %s is not 08:30 on %s in New York", due, str(o, "due_date"))
	}
}

// ---- reminders ----

func TestReminderPayCreatesATransactionAndNothingElseMovesMoney(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 1000000)
	r := e.call(t, "POST", "/v1/reminders", tok, newKey(), map[string]any{"title": "Electricity bill", "amount": 184000, "account_id": a,
		"due_date": dateStr(todayIST().AddDate(0, 0, -1))})
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	id := str(r.data(), "id")
	if str(r.data(), "state") != "due" {
		t.Fatalf("a reminder past its due time is due: %s", r.Raw)
	}
	// Creating, editing, snoozing and deleting never touch a balance.
	snooze := e.do(t, "POST", "/v1/reminders/"+id+"/snooze", tok, map[string]any{"until": time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)})
	if snooze.Status != 200 || str(snooze.data(), "state") != "snoozed" {
		t.Fatalf("snooze: %d %s", snooze.Status, snooze.Raw)
	}
	if e.balance(t, tok, a) != 1000000 {
		t.Fatal("a reminder must not change a balance")
	}
	pay := e.call(t, "POST", "/v1/reminders/"+id+"/pay", tok, newKey(), map[string]any{})
	if pay.Status != 200 || str(pay.data(), "status") != "paid" || pay.data()["transaction_id"] == nil {
		t.Fatalf("pay: %d %s", pay.Status, pay.Raw)
	}
	if e.balance(t, tok, a) != 1000000-184000 {
		t.Fatalf("paying moves money only through the created transaction, balance %d", e.balance(t, tok, a))
	}
	tx := e.do(t, "GET", "/v1/transactions/"+str(pay.data(), "transaction_id"), tok, nil).data()
	if str(tx, "title") != "Electricity bill" || i64(tx, "amount") != 184000 || str(tx, "status") != "confirmed" {
		t.Fatalf("transaction: %v", tx)
	}
	if again := e.call(t, "POST", "/v1/reminders/"+id+"/pay", tok, newKey(), map[string]any{}); again.Status != 409 {
		t.Fatalf("paying twice: %d", again.Status)
	}
	if e.balance(t, tok, a) != 1000000-184000 {
		t.Fatal("a second pay moved money")
	}
	assertLedger(t)
}

func TestReminderPayLinksAnExistingTransactionWithoutMovingMoney(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 500000)
	spent := str(e.spend(t, tok, a, 20000, map[string]any{"title": "Jio Fiber"}).data(), "id")
	rid := str(e.call(t, "POST", "/v1/reminders", tok, newKey(), map[string]any{"title": "Jio Fiber", "due_date": dateStr(todayIST())}).data(), "id")
	before := e.balance(t, tok, a)
	r := e.call(t, "POST", "/v1/reminders/"+rid+"/pay", tok, newKey(), map[string]any{"transaction_id": spent})
	if r.Status != 200 || str(r.data(), "transaction_id") != spent || str(r.data(), "status") != "paid" {
		t.Fatalf("link: %d %s", r.Status, r.Raw)
	}
	if e.balance(t, tok, a) != before {
		t.Fatal("linking an existing transaction must not move money")
	}
	// Paying needs a real transaction: with no amount or account it is refused, not silently marked paid.
	bare := str(e.call(t, "POST", "/v1/reminders", tok, newKey(), map[string]any{"title": "Water", "due_date": dateStr(todayIST())}).data(), "id")
	if p := e.call(t, "POST", "/v1/reminders/"+bare+"/pay", tok, newKey(), map[string]any{}); p.Status != 422 {
		t.Fatalf("pay without details: %d %s", p.Status, p.Raw)
	}
	if g := e.do(t, "GET", "/v1/reminders/"+bare, tok, nil).data(); str(g, "status") != "active" {
		t.Fatal("a refused pay must leave the reminder active")
	}
	// A transaction that is not yours or not an expense cannot be linked.
	income := str(e.earn(t, tok, a, 100).data(), "id")
	if p := e.call(t, "POST", "/v1/reminders/"+bare+"/pay", tok, newKey(), map[string]any{"transaction_id": income}); p.Status != 422 {
		t.Fatalf("income link: %d", p.Status)
	}
	assertLedger(t)
}

func TestRepeatingReminderMovesToNextDueDate(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 1000000)
	due := calendar.Date(2026, time.January, 31)
	r := e.call(t, "POST", "/v1/reminders", tok, newKey(), map[string]any{"title": "Loan EMI", "amount": 1250000, "account_id": a,
		"due_date": dateStr(due), "frequency": "monthly"})
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	id := str(r.data(), "id")
	p1 := e.call(t, "POST", "/v1/reminders/"+id+"/pay", tok, newKey(), map[string]any{"amount": 1000})
	if p1.Status != 200 || str(p1.data(), "status") != "active" || str(p1.data(), "due_date") != "2026-02-28" {
		t.Fatalf("after the 31st the next due date is 28 Feb: %d %s", p1.Status, p1.Raw)
	}
	p2 := e.call(t, "POST", "/v1/reminders/"+id+"/pay", tok, newKey(), map[string]any{"amount": 1000})
	if str(p2.data(), "due_date") != "2026-03-31" {
		t.Fatalf("snaps back to the 31st: %s", p2.Raw)
	}
	if e.balance(t, tok, a) != 1000000-2000 {
		t.Fatal("each payment is its own transaction")
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM transactions WHERE title='Loan EMI'`).Scan(&n)
	if n != 2 {
		t.Fatalf("two cycles, two transactions, got %d", n)
	}
}

func TestReminderPayIsIdempotent(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 100000)
	id := str(e.call(t, "POST", "/v1/reminders", tok, newKey(), map[string]any{"title": "Rent", "amount": 5000, "account_id": a, "due_date": dateStr(todayIST())}).data(), "id")
	key := newKey()
	r1 := e.call(t, "POST", "/v1/reminders/"+id+"/pay", tok, key, map[string]any{})
	r2 := e.call(t, "POST", "/v1/reminders/"+id+"/pay", tok, key, map[string]any{})
	if r1.Status != 200 || r2.Status != 200 || str(r1.data(), "transaction_id") != str(r2.data(), "transaction_id") {
		t.Fatalf("replay: %s | %s", r1.Raw, r2.Raw)
	}
	if e.balance(t, tok, a) != 95000 {
		t.Fatalf("balance %d", e.balance(t, tok, a))
	}
	if r := e.call(t, "POST", "/v1/reminders/"+id+"/pay", tok, "", map[string]any{}); r.Status != 400 {
		t.Fatalf("missing key: %d", r.Status)
	}
}

func TestReminderListOwnershipEditAndDelete(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	a := e.newAccount(t, ta, "A", "savings", 0)
	id := str(e.call(t, "POST", "/v1/reminders", ta, newKey(), map[string]any{"title": "LIC premium", "due_date": dateStr(todayIST().AddDate(0, 0, 10))}).data(), "id")
	if got := e.do(t, "GET", "/v1/reminders?state=upcoming", ta, nil).Body["data"].([]any); len(got) != 1 {
		t.Fatalf("upcoming: %d", len(got))
	}
	if got := e.do(t, "GET", "/v1/reminders?state=due", ta, nil).Body["data"].([]any); len(got) != 0 {
		t.Fatalf("due: %d", len(got))
	}
	if r := e.do(t, "GET", "/v1/reminders?state=bogus", ta, nil); r.Status != 422 {
		t.Fatalf("bad state: %d", r.Status)
	}
	for _, c := range []struct{ m, p string }{{"GET", "/v1/reminders/" + id}, {"DELETE", "/v1/reminders/" + id}} {
		if r := e.call(t, c.m, c.p, tb, "", nil); r.Status != 404 {
			t.Fatalf("%s by another user: %d", c.m, r.Status)
		}
	}
	if r := e.call(t, "POST", "/v1/reminders/"+id+"/pay", tb, newKey(), map[string]any{"account_id": a, "amount": 1}); r.Status != 404 {
		t.Fatalf("pay foreign: %d", r.Status)
	}
	u := e.do(t, "PUT", "/v1/reminders/"+id, ta, map[string]any{"version": 1, "title": "LIC premium (annual)", "amount": 4500000, "account_id": a})
	if u.Status != 200 || str(u.data(), "title") != "LIC premium (annual)" || i64(u.data(), "amount") != 4500000 {
		t.Fatalf("edit: %d %s", u.Status, u.Raw)
	}
	if st := e.do(t, "PUT", "/v1/reminders/"+id, ta, map[string]any{"version": 1, "title": "stale"}); st.Status != 409 {
		t.Fatalf("stale: %d", st.Status)
	}
	if d := e.call(t, "DELETE", "/v1/reminders/"+id, ta, "", nil); d.Status != 204 {
		t.Fatalf("delete: %d", d.Status)
	}
	if got := e.do(t, "GET", "/v1/reminders", ta, nil).Body["data"].([]any); len(got) != 0 {
		t.Fatal("a deleted reminder disappears from the active list")
	}
	if got := e.do(t, "GET", "/v1/reminders?state=cancelled", ta, nil).Body["data"].([]any); len(got) != 1 {
		t.Fatal("deleting keeps the record")
	}
	if e.balance(t, ta, a) != 0 {
		t.Fatal("reminder operations moved money")
	}
}

// ---- budgets ----

func (e *env) budget(t *testing.T, tok string, body map[string]any) resp {
	t.Helper()
	return e.call(t, "POST", "/v1/budgets", tok, newKey(), body)
}

func catID(t *testing.T, e *env, tok, name string) string {
	t.Helper()
	for _, c := range e.do(t, "GET", "/v1/categories", tok, nil).Body["data"].([]any) {
		if m := c.(map[string]any); str(m, "name") == name {
			return str(m, "id")
		}
	}
	t.Fatalf("category %q not found", name)
	return ""
}

func monthStartIST() time.Time { n := todayIST(); return calendar.Date(n.Year(), n.Month(), 1) }

// atIST returns an instant at local midnight of the civil date plus d.
func atIST(date time.Time, d time.Duration) string {
	return time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, ist).Add(d).UTC().Format(time.RFC3339)
}

func TestBudgetSpendIsDerivedFromEligibleTransactions(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 10000000)
	b2 := e.newAccount(t, tok, "SBI", "savings", 0)
	food := catID(t, e, tok, "Food and dining")
	dining := catID(t, e, tok, "Restaurants")
	other := catID(t, e, tok, "Shopping")
	now := atIST(monthStartIST(), 26*time.Hour) // inside this month in IST
	r := e.budget(t, tok, map[string]any{"name": "Dining", "category_id": food, "period": "monthly", "limit": 800000, "alert_percent": 80})
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	id := str(r.data(), "id")
	spend := func(amount int64, extra map[string]any) string {
		m := map[string]any{"occurred_at": now}
		for k, v := range extra {
			m[k] = v
		}
		x := e.spend(t, tok, a, amount, m)
		if x.Status != 201 {
			t.Fatalf("%d %s", x.Status, x.Raw)
		}
		return str(x.data(), "id")
	}
	spend(100000, map[string]any{"category_id": food})                               // counts
	spend(150000, map[string]any{"category_id": dining})                             // subcategory counts
	spend(500000, map[string]any{"category_id": other})                              // other category: ignored
	spend(70000, map[string]any{"category_id": food, "status": "pending"})           // pending counts
	spend(90000, map[string]any{"category_id": food, "status": "needs_review"})      // does not count
	failed := spend(60000, map[string]any{"category_id": food, "status": "pending"}) //
	e.call(t, "POST", "/v1/transactions/"+failed+"/status", tok, newKey(), map[string]any{"status": "failed"})
	e.call(t, "POST", "/v1/transfers", tok, newKey(), map[string]any{"from_account_id": a, "to_account_id": b2, "amount": 400000}) // transfers are not expenses
	e.call(t, "POST", "/v1/accounts/"+b2+"/reconcile", tok, newKey(), map[string]any{"actual_balance": 123})                       // adjustments are not spending
	v := e.do(t, "GET", "/v1/budgets/"+id, tok, nil).data()
	if i64(v, "spent") != 320000 {
		t.Fatalf("spent want 320000 (100000+150000+70000) got %d: %v", i64(v, "spent"), v)
	}
	if i64(v, "effective_limit") != 800000 || i64(v, "remaining") != 480000 || int(i64(v, "percent_used")) != 40 || str(v, "status") != "ok" {
		t.Fatalf("budget numbers: %v", v)
	}
	if v["alert_triggered"] != false {
		t.Fatal("40% is below the 80% alert")
	}
	// Spending past the limit is allowed (limits never block) and shows how far over.
	if over := spend(600000, map[string]any{"category_id": food}); over == "" {
		t.Fatal("limits must never block spending")
	}
	v = e.do(t, "GET", "/v1/budgets/"+id, tok, nil).data()
	if str(v, "status") != "over" || v["over"] != true || i64(v, "over_by") != 120000 || i64(v, "remaining") != -120000 || int(i64(v, "percent_used")) != 115 || v["alert_triggered"] != true {
		t.Fatalf("over budget: %v", v)
	}
	assertLedger(t)
}

func TestBudgetRefundsAreSubtractedAndAccountScopeApplies(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 10000000)
	b := e.newAccount(t, tok, "ICICI", "savings", 10000000)
	now := atIST(monthStartIST(), 30*time.Hour)
	all := str(e.budget(t, tok, map[string]any{"name": "Everything", "period": "monthly", "limit": 1000000}).data(), "id")
	onlyB := str(e.budget(t, tok, map[string]any{"name": "ICICI only", "account_id": b, "period": "monthly", "limit": 1000000}).data(), "id")
	exp := str(e.spend(t, tok, a, 300000, map[string]any{"occurred_at": now}).data(), "id")
	e.spend(t, tok, b, 100000, map[string]any{"occurred_at": now})
	if i64(e.do(t, "GET", "/v1/budgets/"+all, tok, nil).data(), "spent") != 400000 || i64(e.do(t, "GET", "/v1/budgets/"+onlyB, tok, nil).data(), "spent") != 100000 {
		t.Fatal("account scope")
	}
	r := e.call(t, "POST", "/v1/transactions", tok, newKey(), map[string]any{"account_id": a, "type": "income", "amount": 120000, "title": "Refund", "refund_of": exp, "occurred_at": now})
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	if got := i64(e.do(t, "GET", "/v1/budgets/"+all, tok, nil).data(), "spent"); got != 280000 {
		t.Fatalf("a linked refund reduces spend: want 280000 got %d", got)
	}
	// Plain income that is not a refund is not netted against spending.
	e.earn(t, tok, a, 999999)
	if got := i64(e.do(t, "GET", "/v1/budgets/"+all, tok, nil).data(), "spent"); got != 280000 {
		t.Fatalf("unlinked income must not reduce spend: %d", got)
	}
}

func TestBudgetPeriodsUseTheBudgetsTimeZone(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 10000000)
	id := str(e.budget(t, tok, map[string]any{"name": "Month", "period": "monthly", "limit": 1000000, "timezone": "Asia/Kolkata"}).data(), "id")
	m := monthStartIST()
	// 10 minutes after IST midnight is still the previous day in UTC, yet belongs to this month in the budget's zone.
	e.spend(t, tok, a, 11111, map[string]any{"occurred_at": atIST(m, 10*time.Minute)})
	// 10 minutes before IST midnight belongs to last month.
	e.spend(t, tok, a, 22222, map[string]any{"occurred_at": atIST(m, -10*time.Minute)})
	v := e.do(t, "GET", "/v1/budgets/"+id, tok, nil).data()
	if i64(v, "spent") != 11111 {
		t.Fatalf("month boundary in IST: spent %d", i64(v, "spent"))
	}
	if str(v, "period_start") != dateStr(m) {
		t.Fatalf("period start %s want %s", str(v, "period_start"), dateStr(m))
	}
}

func TestBudgetPastPeriodsKeepTheirOriginalLimit(t *testing.T) { // DESIGN.md: past months keep their original limit
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 10000000)
	m := monthStartIST()
	prev := calendar.PrevPeriodStart("monthly", m)
	r := e.budget(t, tok, map[string]any{"name": "Groceries", "period": "monthly", "limit": 1500000, "start_date": dateStr(prev)})
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	id, ver := str(r.data(), "id"), int(i64(r.data(), "version"))
	e.spend(t, tok, a, 1700000, map[string]any{"occurred_at": atIST(prev, 48*time.Hour)}) // last month: over 15,000
	u := e.do(t, "PUT", "/v1/budgets/"+id, tok, map[string]any{"version": ver, "limit": 2000000})
	if u.Status != 200 || i64(u.data(), "limit") != 2000000 {
		t.Fatalf("raise limit: %d %s", u.Status, u.Raw)
	}
	h := e.do(t, "GET", "/v1/budgets/"+id+"/history", tok, nil).Body["data"].([]any)
	if len(h) != 1 {
		t.Fatalf("one past period expected, got %d", len(h))
	}
	p := h[0].(map[string]any)
	if i64(p, "limit") != 1500000 || i64(p, "spent") != 1700000 || p["over"] != true || i64(p, "over_by") != 200000 {
		t.Fatalf("last month keeps its 15,000 limit and its overspend: %v", p)
	}
}

func TestBudgetRollover(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 100000000)
	m := monthStartIST()
	m1 := calendar.PrevPeriodStart("monthly", calendar.PrevPeriodStart("monthly", m)) // two months ago
	m2 := calendar.PrevPeriodStart("monthly", m)
	id := str(e.budget(t, tok, map[string]any{"name": "Travel", "period": "monthly", "limit": 1000000, "rollover": true, "start_date": dateStr(m1)}).data(), "id")
	e.spend(t, tok, a, 400000, map[string]any{"occurred_at": atIST(m1, 48*time.Hour)})  // leaves 6,000
	e.spend(t, tok, a, 1200000, map[string]any{"occurred_at": atIST(m2, 48*time.Hour)}) // limit 10,000 + 6,000 carried = 16,000; leaves 4,000
	v := e.do(t, "GET", "/v1/budgets/"+id, tok, nil).data()
	if i64(v, "carried_over") != 400000 || i64(v, "effective_limit") != 1400000 || i64(v, "spent") != 0 {
		t.Fatalf("current period should carry 4,000 over: %v", v)
	}
	h := e.do(t, "GET", "/v1/budgets/"+id+"/history", tok, nil).Body["data"].([]any)
	if len(h) != 2 || i64(h[1].(map[string]any), "carried_over") != 0 || i64(h[0].(map[string]any), "carried_over") != 600000 || i64(h[0].(map[string]any), "effective_limit") != 1600000 {
		t.Fatalf("history: %v", h)
	}
	// An overspent period carries nothing (a negative leftover does not reduce the next limit).
	e.spend(t, tok, a, 2500000, map[string]any{"occurred_at": atIST(m2, 72*time.Hour)})
	v = e.do(t, "GET", "/v1/budgets/"+id, tok, nil).data()
	if i64(v, "carried_over") != 0 || i64(v, "effective_limit") != 1000000 {
		t.Fatalf("overspend must not carry a negative amount: %v", v)
	}
}

func TestCustomBudgetAndValidation(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 10000000)
	today := todayIST()
	r := e.budget(t, tok, map[string]any{"name": "Goa trip", "period": "custom", "limit": 5000000, "start_date": dateStr(today.AddDate(0, 0, -2)), "end_date": dateStr(today.AddDate(0, 0, 5))})
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	e.spend(t, tok, a, 1250000, nil)
	v := e.do(t, "GET", "/v1/budgets/"+str(r.data(), "id"), tok, nil).data()
	if i64(v, "spent") != 1250000 || int(i64(v, "percent_used")) != 25 || str(v, "status") != "ok" {
		t.Fatalf("custom: %v", v)
	}
	for name, body := range map[string]map[string]any{
		"zero limit":       {"name": "x", "period": "monthly", "limit": 0},
		"no name":          {"name": " ", "period": "monthly", "limit": 5},
		"bad period":       {"name": "x", "period": "daily", "limit": 5},
		"custom, no end":   {"name": "x", "period": "custom", "limit": 5},
		"end before start": {"name": "x", "period": "custom", "limit": 5, "start_date": "2026-05-10", "end_date": "2026-05-01"},
		"custom rollover":  {"name": "x", "period": "custom", "limit": 5, "start_date": "2026-05-01", "end_date": "2026-05-09", "rollover": true},
		"bad alert":        {"name": "x", "period": "monthly", "limit": 5, "alert_percent": 0},
		"bad category":     {"name": "x", "period": "monthly", "limit": 5, "category_id": "nope"},
		"income category":  {"name": "x", "period": "monthly", "limit": 5, "category_id": catID(t, e, tok, "Salary")},
	} {
		if r := e.budget(t, tok, body); r.Status != 422 {
			t.Errorf("%s: want 422 got %d %s", name, r.Status, r.Raw)
		}
	}
}

func TestBudgetOwnershipAndArchive(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	a := e.newAccount(t, ta, "A", "savings", 100000)
	id := str(e.budget(t, ta, map[string]any{"name": "Mine", "period": "monthly", "limit": 5000}).data(), "id")
	if r := e.budget(t, tb, map[string]any{"name": "Theirs", "account_id": a, "period": "monthly", "limit": 5000}); r.Status != 422 {
		t.Fatalf("a budget scoped to a foreign account: %d", r.Status)
	}
	for _, c := range []struct{ m, p string }{{"GET", "/v1/budgets/" + id}, {"GET", "/v1/budgets/" + id + "/history"}, {"POST", "/v1/budgets/" + id + "/archive"}} {
		if r := e.call(t, c.m, c.p, tb, "", nil); r.Status != 404 {
			t.Fatalf("%s by another user: %d", c.m, r.Status)
		}
	}
	if r := e.do(t, "PUT", "/v1/budgets/"+id, tb, map[string]any{"version": 1, "name": "hijack"}); r.Status != 404 {
		t.Fatalf("edit foreign: %d", r.Status)
	}
	if strings.Contains(e.do(t, "GET", "/v1/budgets", tb, nil).Raw, id) {
		t.Fatal("leak")
	}
	if r := e.call(t, "POST", "/v1/budgets/"+id+"/archive", ta, "", nil); r.Status != 200 || r.data()["archived"] != true {
		t.Fatalf("archive: %d %s", r.Status, r.Raw)
	}
	if n := len(e.do(t, "GET", "/v1/budgets", ta, nil).Body["data"].([]any)); n != 0 {
		t.Fatal("archived budgets are hidden by default")
	}
	if n := len(e.do(t, "GET", "/v1/budgets?include_archived=true", ta, nil).Body["data"].([]any)); n != 1 {
		t.Fatal("include_archived shows it")
	}
	if st := e.do(t, "PUT", "/v1/budgets/"+id, ta, map[string]any{"version": 1, "name": "stale"}); st.Status != 409 && st.Status != 200 {
		t.Fatalf("unexpected %d", st.Status)
	}
}

// ---- trackers ----

func TestTrackerAccumulatesWithNoLimit(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 10000000)
	r := e.call(t, "POST", "/v1/trackers", tok, newKey(), map[string]any{"name": "Goa trip"})
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	id := str(r.data(), "id")
	if i64(r.data(), "total") != 0 || i64(r.data(), "transaction_count") != 0 {
		t.Fatalf("a tracker starts at zero: %s", r.Raw)
	}
	// A tracker must never look like a budget: no limit, remaining or percentage anywhere.
	for _, forbidden := range []string{"limit", "remaining", "percent", "over_by", "budget"} {
		if strings.Contains(strings.ToLower(r.Raw), forbidden) {
			t.Fatalf("tracker response contains %q: %s", forbidden, r.Raw)
		}
	}
	t1 := str(e.spend(t, tok, a, 450000, map[string]any{"title": "Flights", "occurred_at": atIST(todayIST().AddDate(0, 0, -3), 3*time.Hour)}).data(), "id")
	t2 := str(e.spend(t, tok, a, 120000, map[string]any{"title": "Hotel", "occurred_at": atIST(todayIST().AddDate(0, 0, -1), 3*time.Hour)}).data(), "id")
	e.spend(t, tok, a, 999, map[string]any{"title": "Unrelated"})
	add := e.call(t, "POST", "/v1/trackers/"+id+"/transactions", tok, "", map[string]any{"transaction_ids": []string{t1, t2}})
	if add.Status != 200 || i64(add.data(), "total") != 570000 || i64(add.data(), "transaction_count") != 2 {
		t.Fatalf("add: %d %s", add.Status, add.Raw)
	}
	series := add.data()["series"].([]any)
	if len(series) != 2 || i64(series[0].(map[string]any), "total") != 450000 || i64(series[1].(map[string]any), "total") != 570000 {
		t.Fatalf("running total: %v", series)
	}
	// Adding again is harmless. A refund of a linked expense reduces the total.
	e.call(t, "POST", "/v1/trackers/"+id+"/transactions", tok, "", map[string]any{"transaction_ids": []string{t1}})
	e.call(t, "POST", "/v1/transactions", tok, newKey(), map[string]any{"account_id": a, "type": "income", "amount": 50000, "title": "Refund", "refund_of": t2})
	g := e.do(t, "GET", "/v1/trackers/"+id, tok, nil).data()
	if i64(g, "total") != 520000 {
		t.Fatalf("refund: %v", g)
	}
	// Voiding a linked expense drops it from the total; the transaction itself is unchanged by tracker operations.
	list := e.do(t, "GET", "/v1/trackers/"+id+"/transactions", tok, nil).Body["data"].([]any)
	if len(list) != 2 {
		t.Fatalf("tracker transactions: %d", len(list))
	}
	if rm := e.call(t, "DELETE", "/v1/trackers/"+id+"/transactions/"+t1, tok, "", nil); rm.Status != 200 || i64(rm.data(), "total") != 70000 {
		t.Fatalf("remove: %d %s", rm.Status, rm.Raw)
	}
	if e.do(t, "GET", "/v1/transactions/"+t1, tok, nil).Status != 200 {
		t.Fatal("removing from a tracker must not delete the transaction")
	}
	assertLedger(t)
}

func TestTrackerRules(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	a := e.newAccount(t, ta, "A", "savings", 1000000)
	b := e.newAccount(t, tb, "B", "savings", 1000000)
	usd := str(e.call(t, "POST", "/v1/accounts", ta, newKey(), map[string]any{"name": "USD", "type": "savings", "currency": "USD"}).data(), "id")
	mine := str(e.spend(t, ta, a, 1000, nil).data(), "id")
	theirs := str(e.spend(t, tb, b, 1000, nil).data(), "id")
	income := str(e.earn(t, ta, a, 1000).data(), "id")
	dollars := str(e.spend(t, ta, usd, 1000, nil).data(), "id")
	id := str(e.call(t, "POST", "/v1/trackers", ta, newKey(), map[string]any{"name": "Home"}).data(), "id")
	for name, tx := range map[string]string{"foreign": theirs, "income": income, "other currency": dollars} {
		if r := e.call(t, "POST", "/v1/trackers/"+id+"/transactions", ta, "", map[string]any{"transaction_ids": []string{mine, tx}}); r.Status != 422 {
			t.Errorf("%s: want 422 got %d %s", name, r.Status, r.Raw)
		}
	}
	if i64(e.do(t, "GET", "/v1/trackers/"+id, ta, nil).data(), "transaction_count") != 0 {
		t.Fatal("a rejected batch must add nothing (all or none)")
	}
	for _, c := range []struct{ m, p string }{{"GET", "/v1/trackers/" + id}, {"POST", "/v1/trackers/" + id + "/archive"}, {"GET", "/v1/trackers/" + id + "/transactions"}} {
		if r := e.call(t, c.m, c.p, tb, "", nil); r.Status != 404 {
			t.Errorf("%s by another user: %d", c.m, r.Status)
		}
	}
	if r := e.call(t, "POST", "/v1/trackers/"+id+"/transactions", tb, "", map[string]any{"transaction_ids": []string{theirs}}); r.Status != 404 {
		t.Fatalf("add to a foreign tracker: %d", r.Status)
	}
	if r := e.call(t, "POST", "/v1/trackers/"+id+"/archive", ta, "", nil); r.Status != 200 || r.data()["archived"] != true {
		t.Fatalf("archive: %d %s", r.Status, r.Raw)
	}
	if r := e.call(t, "POST", "/v1/trackers/"+id+"/transactions", ta, "", map[string]any{"transaction_ids": []string{mine}}); r.Status != 409 {
		t.Fatalf("archived tracker accepts nothing: %d", r.Status)
	}
	if r := e.call(t, "POST", "/v1/trackers", ta, newKey(), map[string]any{"name": ""}); r.Status != 422 {
		t.Fatalf("name: %d", r.Status)
	}
}

func TestTrackerExcludesVoidedExpenses(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 1000000)
	id := str(e.call(t, "POST", "/v1/trackers", tok, newKey(), map[string]any{"name": "Reno"}).data(), "id")
	x := str(e.spend(t, tok, a, 40000, nil).data(), "id")
	e.call(t, "POST", "/v1/trackers/"+id+"/transactions", tok, "", map[string]any{"transaction_ids": []string{x}})
	e.call(t, "POST", "/v1/transactions/"+x+"/void", tok, newKey(), nil)
	if g := e.do(t, "GET", "/v1/trackers/"+id, tok, nil).data(); i64(g, "total") != 0 || i64(g, "transaction_count") != 0 {
		t.Fatalf("a voided expense must drop out of the total: %v", g)
	}
	_ = fmt.Sprint
}

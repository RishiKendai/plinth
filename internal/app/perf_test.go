package app_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

// seedMany inserts n confirmed transactions straight into the table (bypassing the ledger on purpose: this measures read
// queries over a large history, not writes).
func seedMany(t *testing.T, userID, acct string, n int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO transactions (id, user_id, account_id, type, amount, currency, title, status, occurred_at, timezone, source, category_id, payment_method, payee_name)
		SELECT gen_random_uuid(), $1, $2, CASE WHEN g % 20 = 0 THEN 'income' ELSE 'expense' END, 100 + (g * 37) % 90000, 'INR', 'Item ' || (g % 500), 'confirmed',
		       '2023-11-01T00:00:00Z'::timestamptz + (g * interval '15 minutes'), 'Asia/Kolkata', 'manual',
		       (SELECT id FROM categories WHERE user_id IS NULL AND kind = 'expense' ORDER BY id OFFSET (g % 8) LIMIT 1),
		       (ARRAY['','upi','cash','debit_card'])[1 + g % 4], 'Shop ' || (g % 300)
		FROM generate_series(1, $3) g`, userID, acct, n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `ANALYZE transactions`); err != nil {
		t.Fatal(err)
	}
}

// TestAnalyticsOnALargeHistory is the Phase 6 performance check: 150,000 transactions (about 4 years of heavy use) must
// answer every analytics query well inside an interactive budget. The threshold is deliberately loose (shared CI machines);
// the printed timings and plan are what docs/metrics.md quotes.
func TestAnalyticsOnALargeHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("large dataset")
	}
	e := setup(t)
	tok, _, uid := e.signUp(t, "big@example.com")
	acct := e.newAccount(t, tok, "HDFC", "savings", 0)
	seedMany(t, uid, acct, 150_000)

	budget := 1500 * time.Millisecond
	for _, q := range []string{
		"/v1/analytics/overview?from=2025-01-01&to=2025-12-31",
		"/v1/analytics/overview?from=2026-10-01&to=2026-10-31",
		"/v1/analytics/expenses?from=2025-01-01&to=2025-12-31",
		"/v1/analytics/expenses?by=merchant&from=2023-11-01&to=2027-01-01",
		"/v1/analytics/trends?from=2023-11-01&to=2027-01-01&granularity=month",
		"/v1/analytics/trends?from=2026-01-01&to=2026-12-31&granularity=day",
		"/v1/analytics/compare?from=2026-01-01&to=2026-12-31",
		"/v1/analytics/heatmap?from=2026-01-01&to=2026-12-31",
		"/v1/insights?from=2026-01-01&to=2026-12-31",
	} {
		start := time.Now()
		r := e.do(t, "GET", q, tok, nil)
		d := time.Since(start)
		t.Logf("%-85s %4d ms", q, d.Milliseconds())
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", q, r.Status, r.Raw)
		}
		if d > budget {
			t.Errorf("%s took %v, budget %v", q, d, budget)
		}
	}

	// The plan for the most common shape: one month of facts. It must use an index, not scan 150,000 rows. (A full-year range
	// reads about a quarter of the table, so the planner correctly prefers a parallel sequential scan there; that still ran in
	// well under 100 ms, so no further index was added.)
	rows, err := pool.Query(context.Background(), `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) SELECT sum(COALESCE(counted_amount, amount)) FROM transactions
		WHERE user_id=$1 AND currency='INR' AND type IN ('expense','income') AND refund_of IS NULL AND status IN ('pending','confirmed')
		AND source NOT IN ('reconcile','settlement') AND occurred_at >= '2025-06-01' AND occurred_at < '2025-07-01'`, uid)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var l string
		_ = rows.Scan(&l)
		plan = append(plan, l)
	}
	rows.Close()
	t.Log("\n" + strings.Join(plan, "\n"))
	if p := strings.Join(plan, "\n"); !strings.Contains(p, "Index") && !strings.Contains(p, "Bitmap") {
		t.Errorf("a date-range analytics query must use an index:\n%s", p)
	}
}

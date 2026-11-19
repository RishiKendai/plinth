// Package analytics answers "where does my money come from and go" from the ledger's transactions. Every metric is built
// on one definition of an eligible transaction (see facts) so no two screens can disagree. Definitions: docs/metrics.md.
package analytics

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/ids"
	"plinth/server/internal/settings"
)

type Service struct {
	db       *pgxpool.Pool
	settings *settings.Service
	now      func() time.Time
}

func NewService(db *pgxpool.Pool, st *settings.Service) *Service {
	return &Service{db: db, settings: st, now: time.Now}
}

// Scope is the resolved question: whose data, which currency, which instants, which day boundaries.
type Scope struct {
	UserID    string
	Currency  string
	TZ        string
	Loc       *time.Location
	From, To  time.Time // instants, To exclusive
	FromDate  string    // inclusive local dates, echoed back
	ToDate    string
	AccountID *string
}

// Days is the number of local calendar days in the range.
func (s Scope) Days() int {
	f, _ := time.Parse("2006-01-02", s.FromDate)
	t, _ := time.Parse("2006-01-02", s.ToDate)
	return int(t.Sub(f).Hours()/24) + 1
}

const maxRangeDays = 5 * 366

// Resolve turns query values into a Scope. Empty from/to means the current month in the user's timezone. Currency defaults
// to the user's own; amounts in different currencies are never added together.
func (s *Service) Resolve(ctx context.Context, userID, from, to, currency, accountID string) (Scope, error) {
	st, err := s.settings.Get(ctx, userID)
	if err != nil {
		return Scope{}, err
	}
	loc, err := time.LoadLocation(st.Timezone)
	if err != nil {
		loc = time.UTC
	}
	sc := Scope{UserID: userID, Currency: st.Currency, TZ: loc.String(), Loc: loc}
	if currency != "" {
		sc.Currency = strings.ToUpper(currency)
	}
	now := s.now().In(loc)
	f, t := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	fields := map[string]string{}
	if from != "" {
		if v, err := time.Parse("2006-01-02", from); err != nil {
			fields["from"] = "Use a date like 2026-11-01."
		} else {
			f = v
			if to == "" {
				t = v
				if t.Before(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)) {
					t = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
				}
			}
		}
	}
	if to != "" {
		if v, err := time.Parse("2006-01-02", to); err != nil {
			fields["to"] = "Use a date like 2026-11-30."
		} else {
			t = v
		}
	}
	if len(fields) == 0 && t.Before(f) {
		fields["to"] = "The end date is before the start date."
	}
	if len(fields) == 0 && int(t.Sub(f).Hours()/24) > maxRangeDays {
		fields["to"] = "Choose a range of 5 years or less."
	}
	if accountID != "" {
		id := ids.Normalize(accountID)
		if !ids.Valid(id) {
			fields["account_id"] = "Choose one of your accounts."
		} else {
			var one int
			if err := s.db.QueryRow(ctx, `SELECT 1 FROM accounts WHERE id=$1 AND user_id=$2`, id, userID).Scan(&one); err != nil {
				return Scope{}, apierr.ErrNotFound
			}
			sc.AccountID = &id
		}
	}
	if len(fields) > 0 {
		return Scope{}, apierr.Validation(fields)
	}
	sc.FromDate, sc.ToDate = f.Format("2006-01-02"), t.Format("2006-01-02")
	sc.From = time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, loc)
	sc.To = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
	return sc, nil
}

// args are the positional parameters every query built on factsCTE shares.
func (sc Scope) args() []any {
	var acct any
	if sc.AccountID != nil {
		acct = *sc.AccountID
	}
	return []any{sc.UserID, sc.Currency, sc.From, sc.To, sc.TZ, acct}
}

// factsCTE is THE definition of spending and income (docs/metrics.md):
//   - type expense or income, status pending or confirmed (the statuses that count toward balances);
//   - never transfers, never balance adjustments (reconcile) or group repayments (settlement);
//   - a group expense counts only the user's own share (counted_amount);
//   - a refund is not income: it reduces the expense it refunds, in that expense's period.
//
// n is 1 for a transaction and 0 for a refund row, so counts are counts of real transactions.
const factsCTE = `WITH facts AS (
	SELECT t.id, t.type, t.category_id, t.account_id, t.payment_method, t.payee_name, t.title, t.occurred_at,
	       (t.occurred_at AT TIME ZONE $5::text)::date AS d, COALESCE(t.counted_amount, t.amount) AS amt, 1 AS n
	FROM transactions t
	WHERE t.user_id = $1 AND t.currency = $2 AND t.type IN ('expense','income') AND t.refund_of IS NULL
	  AND t.status IN ('pending','confirmed') AND t.source NOT IN ('reconcile','settlement','topup')
	  AND t.occurred_at >= $3 AND t.occurred_at < $4 AND ($6::uuid IS NULL OR t.account_id = $6::uuid)
	UNION ALL
	SELECT e.id, 'expense', e.category_id, e.account_id, e.payment_method, e.payee_name, e.title, e.occurred_at,
	       (e.occurred_at AT TIME ZONE $5::text)::date, -LEAST(r.amount, COALESCE(e.counted_amount, e.amount)), 0
	FROM transactions r JOIN transactions e ON e.id = r.refund_of
	WHERE r.user_id = $1 AND r.currency = $2 AND r.status IN ('pending','confirmed')
	  AND e.status IN ('pending','confirmed') AND e.type = 'expense' AND e.source NOT IN ('reconcile','settlement','topup')
	  AND e.occurred_at >= $3 AND e.occurred_at < $4 AND ($6::uuid IS NULL OR e.account_id = $6::uuid)
)`

type Totals struct {
	Income  int64 `json:"income"`
	Expense int64 `json:"expense"`
	Net     int64 `json:"net"`
	Count   int   `json:"count"`
}

func (s *Service) totals(ctx context.Context, sc Scope) (Totals, error) {
	var t Totals
	err := s.db.QueryRow(ctx, factsCTE+`SELECT COALESCE(SUM(amt) FILTER (WHERE type='income'),0)::bigint, COALESCE(SUM(amt) FILTER (WHERE type='expense'),0)::bigint,
		COALESCE(SUM(n),0)::int FROM facts`, sc.args()...).Scan(&t.Income, &t.Expense, &t.Count)
	t.Net = t.Income - t.Expense
	return t, err
}

type Expected struct {
	Income  int64 `json:"income"`
	Expense int64 `json:"expense"`
}

type Overview struct {
	From            string   `json:"from"`
	To              string   `json:"to"`
	Currency        string   `json:"currency"`
	Days            int      `json:"days"`
	Current         Totals   `json:"current"`
	Previous        Totals   `json:"previous"`
	PrevFrom        string   `json:"previous_from"`
	PrevTo          string   `json:"previous_to"`
	SavingsRateBP   *int64   `json:"savings_rate_bp"` // net / income x 10000; null when there was no income
	AvgDailyExpense int64    `json:"avg_daily_expense"`
	Expected        Expected `json:"expected"` // scheduled but not yet recorded; never mixed into current
	EnoughData      bool     `json:"enough_data"`
}

// previous returns the equally long range that ends the day before sc starts.
func previous(sc Scope) Scope {
	p := sc
	days := sc.Days()
	f, _ := time.Parse("2006-01-02", sc.FromDate)
	pt := f.AddDate(0, 0, -1)
	pf := pt.AddDate(0, 0, -(days - 1))
	p.FromDate, p.ToDate = pf.Format("2006-01-02"), pt.Format("2006-01-02")
	p.From = time.Date(pf.Year(), pf.Month(), pf.Day(), 0, 0, 0, 0, sc.Loc)
	p.To = time.Date(pt.Year(), pt.Month(), pt.Day()+1, 0, 0, 0, 0, sc.Loc)
	return p
}

func (s *Service) Overview(ctx context.Context, sc Scope) (Overview, error) {
	cur, err := s.totals(ctx, sc)
	if err != nil {
		return Overview{}, err
	}
	pv := previous(sc)
	prev, err := s.totals(ctx, pv)
	if err != nil {
		return Overview{}, err
	}
	o := Overview{From: sc.FromDate, To: sc.ToDate, Currency: sc.Currency, Days: sc.Days(), Current: cur, Previous: prev, PrevFrom: pv.FromDate, PrevTo: pv.ToDate,
		EnoughData: cur.Count >= 3}
	if cur.Income > 0 {
		r := cur.Net * 10000 / cur.Income
		o.SavingsRateBP = &r
	}
	o.AvgDailyExpense = cur.Expense / int64(o.Days)
	// Expected: occurrences still expected inside the range. Kept apart from actuals on purpose.
	err = s.db.QueryRow(ctx, `SELECT COALESCE(SUM(o.expected_amount) FILTER (WHERE sc.type='income'),0)::bigint, COALESCE(SUM(o.expected_amount) FILTER (WHERE sc.type='expense'),0)::bigint
		FROM recurring_occurrences o JOIN recurring_schedules sc ON sc.id=o.schedule_id
		WHERE o.user_id=$1 AND o.status='expected' AND sc.currency=$2 AND o.due_at >= $3 AND o.due_at < $4 AND ($5::uuid IS NULL OR sc.account_id=$5::uuid)`,
		sc.args()[0], sc.Currency, sc.From, sc.To, sc.args()[5]).Scan(&o.Expected.Income, &o.Expected.Expense)
	return o, err
}

type Row struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Amount  int64  `json:"amount"`
	Count   int    `json:"count"`
	Percent int64  `json:"percent_bp"` // share of the listed total, basis points
}

// breakdownSQL maps the grouping to the key and label expressions over facts f.
var breakdownSQL = map[string]struct{ join, key, label string }{
	"category": {`LEFT JOIN categories c ON c.id = f.category_id LEFT JOIN categories p ON p.id = COALESCE(c.parent_id, c.id)`,
		`COALESCE(p.id::text, '')`, `COALESCE(p.name, 'Uncategorised')`},
	"merchant":       {``, `lower(COALESCE(NULLIF(btrim(f.payee_name),''), btrim(f.title)))`, `min(COALESCE(NULLIF(btrim(f.payee_name),''), btrim(f.title)))`},
	"payment_method": {``, `f.payment_method`, `f.payment_method`},
	"account":        {`JOIN accounts a ON a.id = f.account_id`, `f.account_id::text`, `min(a.name)`},
	"tag":            {`JOIN transaction_tags tt ON tt.transaction_id = f.id JOIN tags g ON g.id = tt.tag_id`, `g.id::text`, `min(g.name)`},
}

func Groupings() []string {
	return []string{"category", "merchant", "payment_method", "account", "tag"}
}

// Breakdown groups eligible transactions of one type. A transaction with two tags appears under both tags (so tag rows can
// add up to more than the total); every other grouping adds up exactly. At most limit rows are returned; the rest are
// folded into one "Other" row so the rows still sum to the total.
func (s *Service) Breakdown(ctx context.Context, sc Scope, by, typ string, limit int) ([]Row, int64, error) {
	b, ok := breakdownSQL[by]
	if !ok {
		return nil, 0, apierr.Validation(map[string]string{"by": "Choose category, merchant, payment_method, account or tag."})
	}
	if typ != "expense" && typ != "income" {
		return nil, 0, apierr.Validation(map[string]string{"type": "Choose expense or income."})
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	labelExpr := b.label
	if by == "category" || by == "payment_method" {
		labelExpr = "min(" + b.label + ")"
	}
	q := factsCTE + ` SELECT ` + b.key + `, ` + labelExpr + `, COALESCE(SUM(f.amt),0)::bigint, COALESCE(SUM(f.n),0)::int FROM facts f ` + b.join +
		` WHERE f.type = '` + typ + `' GROUP BY 1 HAVING SUM(f.amt) <> 0 ORDER BY 3 DESC, 1`
	rows, err := s.db.Query(ctx, q, sc.args()...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var all []Row
	var total int64
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Key, &r.Label, &r.Amount, &r.Count); err != nil {
			return nil, 0, err
		}
		if by == "payment_method" && r.Key == "" {
			r.Label = "Not specified"
		}
		if by == "merchant" {
			r.Label = strings.TrimSpace(r.Label)
		}
		total += r.Amount
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(all) > limit {
		rest := Row{Key: "other", Label: "Other"}
		for _, r := range all[limit-1:] {
			rest.Amount += r.Amount
			rest.Count += r.Count
		}
		all = append(all[:limit-1], rest)
	}
	for i := range all {
		if total > 0 {
			all[i].Percent = all[i].Amount * 10000 / total
		}
	}
	return all, total, nil
}

type Point struct {
	Date       string `json:"date"` // first local day of the bucket
	Income     int64  `json:"income"`
	Expense    int64  `json:"expense"`
	Net        int64  `json:"net"`
	Cumulative int64  `json:"cumulative_net"`
}

// Trend buckets by day, week (Monday start) or month in the user's timezone. Empty buckets are returned as zeros so a chart
// has no gaps.
func (s *Service) Trend(ctx context.Context, sc Scope, gran string) ([]Point, error) {
	var trunc string
	switch gran {
	case "day", "":
		trunc, gran = "day", "day"
	case "week":
		trunc = "week"
	case "month":
		trunc = "month"
	default:
		return nil, apierr.Validation(map[string]string{"granularity": "Choose day, week or month."})
	}
	rows, err := s.db.Query(ctx, factsCTE+` SELECT to_char(date_trunc('`+trunc+`', f.d::timestamp)::date, 'YYYY-MM-DD'),
		COALESCE(SUM(f.amt) FILTER (WHERE f.type='income'),0)::bigint, COALESCE(SUM(f.amt) FILTER (WHERE f.type='expense'),0)::bigint FROM facts f GROUP BY 1 ORDER BY 1`, sc.args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	got := map[string]Point{}
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.Date, &p.Income, &p.Expense); err != nil {
			return nil, err
		}
		got[p.Date] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Fill every bucket in the range.
	f, _ := time.Parse("2006-01-02", sc.FromDate)
	t, _ := time.Parse("2006-01-02", sc.ToDate)
	cur := f
	switch gran {
	case "week":
		for cur.Weekday() != time.Monday {
			cur = cur.AddDate(0, 0, -1)
		}
	case "month":
		cur = time.Date(f.Year(), f.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	var out []Point
	var cum int64
	for !cur.After(t) {
		k := cur.Format("2006-01-02")
		p := got[k]
		p.Date = k
		p.Net = p.Income - p.Expense
		cum += p.Net
		p.Cumulative = cum
		out = append(out, p)
		switch gran {
		case "day":
			cur = cur.AddDate(0, 0, 1)
		case "week":
			cur = cur.AddDate(0, 0, 7)
		default:
			cur = cur.AddDate(0, 1, 0)
		}
	}
	return out, nil
}

type CompareRow struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Current  int64  `json:"current"`
	Previous int64  `json:"previous"`
	Change   int64  `json:"change"`
}

// Compare lines up each category's expense in this range against the equally long range before it.
func (s *Service) Compare(ctx context.Context, sc Scope) ([]CompareRow, string, string, error) {
	cur, _, err := s.Breakdown(ctx, sc, "category", "expense", 100)
	if err != nil {
		return nil, "", "", err
	}
	pv := previous(sc)
	prev, _, err := s.Breakdown(ctx, pv, "category", "expense", 100)
	if err != nil {
		return nil, "", "", err
	}
	m := map[string]*CompareRow{}
	for _, r := range cur {
		m[r.Key] = &CompareRow{Key: r.Key, Label: r.Label, Current: r.Amount}
	}
	for _, r := range prev {
		if x, ok := m[r.Key]; ok {
			x.Previous = r.Amount
		} else {
			m[r.Key] = &CompareRow{Key: r.Key, Label: r.Label, Previous: r.Amount}
		}
	}
	out := make([]CompareRow, 0, len(m))
	for _, x := range m {
		x.Change = x.Current - x.Previous
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Current != out[j].Current {
			return out[i].Current > out[j].Current
		}
		return out[i].Key < out[j].Key
	})
	return out, pv.FromDate, pv.ToDate, nil
}

type Heat struct {
	Cells   []Cell   `json:"cells"`
	Max     int64    `json:"max"`
	Weekday [7]int64 `json:"weekday_total"` // Monday first
}
type Cell struct {
	Date   string `json:"date"`
	Amount int64  `json:"amount"`
}

// Heatmap is expense per local day, plus the total for each weekday.
func (s *Service) Heatmap(ctx context.Context, sc Scope) (Heat, error) {
	pts, err := s.Trend(ctx, sc, "day")
	if err != nil {
		return Heat{}, err
	}
	var h Heat
	for _, p := range pts {
		h.Cells = append(h.Cells, Cell{p.Date, p.Expense})
		if p.Expense > h.Max {
			h.Max = p.Expense
		}
		d, _ := time.Parse("2006-01-02", p.Date)
		h.Weekday[(int(d.Weekday())+6)%7] += p.Expense
	}
	return h, nil
}

// ---- account analytics ----

type AccountAnalytics struct {
	AccountID  string      `json:"account_id"`
	Currency   string      `json:"currency"`
	From       string      `json:"from"`
	To         string      `json:"to"`
	Opening    int64       `json:"opening_balance"` // balance at the start of the range
	Closing    int64       `json:"closing_balance"`
	Inflow     int64       `json:"inflow"`  // everything that entered the account, transfers included
	Outflow    int64       `json:"outflow"` // everything that left it, transfers included
	History    []BalancePt `json:"history"` // end-of-day balance
	Categories []Row       `json:"categories"`
}
type BalancePt struct {
	Date    string `json:"date"`
	Balance int64  `json:"balance"`
}

// Account is the ledger view of one account. Unlike spending metrics it includes transfers and adjustments, because the
// question is "how did this balance move". Balances come from opening balance plus ledger entries, never the cached balance.
func (s *Service) Account(ctx context.Context, userID, accountID, from, to string) (AccountAnalytics, error) {
	sc, err := s.Resolve(ctx, userID, from, to, "", accountID)
	if err != nil {
		return AccountAnalytics{}, err
	}
	var out AccountAnalytics
	var opening int64
	if err := s.db.QueryRow(ctx, `SELECT currency, opening_balance FROM accounts WHERE id=$1 AND user_id=$2`, *sc.AccountID, userID).Scan(&out.Currency, &opening); err != nil {
		return out, apierr.ErrNotFound
	}
	sc.Currency = out.Currency
	out.AccountID, out.From, out.To = *sc.AccountID, sc.FromDate, sc.ToDate
	var before int64
	if err := s.db.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0)::bigint FROM ledger_entries WHERE account_id=$1 AND user_id=$2 AND effective_at < $3`, *sc.AccountID, userID, sc.From).Scan(&before); err != nil {
		return out, err
	}
	out.Opening = opening + before
	// Net per day uses every entry (a void's reversal cancels its original). Inflow and outflow count only transactions that
	// still stand, so a transaction later cancelled does not inflate both sides.
	rows, err := s.db.Query(ctx, `SELECT to_char((e.effective_at AT TIME ZONE $4::text)::date,'YYYY-MM-DD'), SUM(e.amount)::bigint,
		COALESCE(SUM(e.amount) FILTER (WHERE e.amount>0 AND t.status IN ('pending','confirmed')),0)::bigint,
		COALESCE(-SUM(e.amount) FILTER (WHERE e.amount<0 AND t.status IN ('pending','confirmed')),0)::bigint
		FROM ledger_entries e JOIN transactions t ON t.id = e.transaction_id
		WHERE e.account_id=$1 AND e.user_id=$2 AND e.effective_at >= $3 AND e.effective_at < $5 GROUP BY 1`, *sc.AccountID, userID, sc.From, sc.TZ, sc.To)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	day := map[string]int64{}
	for rows.Next() {
		var d string
		var net, in, outf int64
		if err := rows.Scan(&d, &net, &in, &outf); err != nil {
			return out, err
		}
		day[d] = net
		out.Inflow += in
		out.Outflow += outf
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	bal := out.Opening
	f, _ := time.Parse("2006-01-02", sc.FromDate)
	t, _ := time.Parse("2006-01-02", sc.ToDate)
	for d := f; !d.After(t); d = d.AddDate(0, 0, 1) {
		k := d.Format("2006-01-02")
		bal += day[k]
		out.History = append(out.History, BalancePt{k, bal})
	}
	out.Closing = bal
	out.Categories, _, err = s.Breakdown(ctx, sc, "category", "expense", 8)
	return out, err
}

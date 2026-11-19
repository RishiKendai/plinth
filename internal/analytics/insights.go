package analytics

import (
	"context"
	"fmt"
	"time"

	"plinth/server/internal/money"
)

// Insight is a plain statement about the past. It never recommends anything (DESIGN.md section 12). Text may contain the
// tokens {amount} and {amount2}; the app fills them in so privacy mode can hide figures.
type Insight struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Amount   int64  `json:"amount"`
	Amount2  int64  `json:"amount2"`
	Currency string `json:"currency"`
}

// Insights derives a handful of descriptive statements for the range. Every figure comes from the same facts as the
// charts. Fewer than 3 transactions is "not enough data": an empty list, never invented text.
func (s *Service) Insights(ctx context.Context, sc Scope) ([]Insight, error) {
	ov, err := s.Overview(ctx, sc)
	if err != nil {
		return nil, err
	}
	if !ov.EnoughData {
		return []Insight{}, nil
	}
	var out []Insight
	add := func(id, kind, text string, a, b int64) {
		out = append(out, Insight{ID: id, Kind: kind, Text: text, Amount: a, Amount2: b, Currency: sc.Currency})
	}
	// Spending against the previous equal period.
	if ov.Previous.Expense > 0 && ov.Current.Expense > 0 {
		diff := ov.Current.Expense - ov.Previous.Expense
		pct := abs64(diff) * 100 / ov.Previous.Expense
		switch {
		case pct == 0:
			add("spend-vs-prev", "spending", "Spending was about the same as the period before: {amount}.", ov.Current.Expense, ov.Previous.Expense)
		case diff > 0:
			add("spend-vs-prev", "spending", fmt.Sprintf("You spent {amount}, which is %d%% more than the period before ({amount2}).", pct), ov.Current.Expense, ov.Previous.Expense)
		default:
			add("spend-vs-prev", "spending", fmt.Sprintf("You spent {amount}, which is %d%% less than the period before ({amount2}).", pct), ov.Current.Expense, ov.Previous.Expense)
		}
	}
	// Biggest category and its share.
	cats, total, err := s.Breakdown(ctx, sc, "category", "expense", 5)
	if err != nil {
		return nil, err
	}
	if len(cats) > 0 && total > 0 {
		add("top-category", "category", fmt.Sprintf("%s was your largest category: {amount}, %d%% of spending.", cats[0].Label, cats[0].Percent/100), cats[0].Amount, total)
	}
	// Category with the largest increase.
	if cmp, _, _, err := s.Compare(ctx, sc); err == nil {
		var best *CompareRow
		for i := range cmp {
			c := &cmp[i]
			if c.Previous > 0 && c.Change > 0 && (best == nil || c.Change > best.Change) {
				best = c
			}
		}
		if best != nil && best.Change*10 >= best.Previous { // at least 10% up
			add("category-up", "category", fmt.Sprintf("%s rose by {amount} compared with the period before.", best.Label), best.Change, best.Current)
		}
	}
	// Single largest expense.
	var title string
	var amt int64
	var at time.Time
	err = s.db.QueryRow(ctx, factsCTE+` SELECT title, amt, occurred_at FROM facts WHERE type='expense' AND n=1 ORDER BY amt DESC, id LIMIT 1`, sc.args()...).Scan(&title, &amt, &at)
	if err == nil {
		add("largest-expense", "expense", fmt.Sprintf("Your largest single expense was %s on %s: {amount}.", title, at.In(sc.Loc).Format("2 Jan")), amt, 0)
	}
	// Money in against money out.
	switch {
	case ov.Current.Income > 0 && ov.Current.Net >= 0:
		add("net", "cashflow", "More came in than went out: {amount} in, {amount2} out.", ov.Current.Income, ov.Current.Expense)
	case ov.Current.Income > 0:
		add("net", "cashflow", "More went out than came in: {amount} out, {amount2} in.", ov.Current.Expense, ov.Current.Income)
	}
	// Days with no spending (only for ranges that have fully or partly passed).
	pts, err := s.Trend(ctx, sc, "day")
	if err == nil {
		today := s.now().In(sc.Loc).Format("2006-01-02")
		var days, none int
		for _, p := range pts {
			if p.Date > today {
				continue
			}
			days++
			if p.Expense == 0 {
				none++
			}
		}
		if days >= 7 && none > 0 {
			add("no-spend", "spending", fmt.Sprintf("You had %d days with no spending out of %d.", none, days), int64(none), int64(days))
		}
	}
	return out, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// plain renders minor units as a plain decimal such as "-1250.50" for CSV files (no symbol, no grouping).
func plain(minor int64, currency string) string {
	c, ok := money.Lookup(currency)
	if !ok {
		c, _ = money.Lookup("INR")
	}
	neg := minor < 0
	mag := uint64(minor)
	if neg {
		mag = uint64(-(minor + 1)) + 1
	}
	div := uint64(1)
	for i := 0; i < c.Exponent; i++ {
		div *= 10
	}
	s := fmt.Sprintf("%d", mag/div)
	if c.Exponent > 0 {
		s += fmt.Sprintf(".%0*d", c.Exponent, mag%div)
	}
	if neg {
		s = "-" + s
	}
	return s
}

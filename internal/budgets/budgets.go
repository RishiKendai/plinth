// Package budgets owns spending limits (CLAUDE.md 3.9). A budget never stores what was spent: spending is derived
// from eligible transactions every time, so it cannot drift from the ledger. Rules are in docs/budgets.md.
//
// A budget is a limit, not a block: nothing here ever prevents or alters a transaction.
package budgets

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/accounts"
	"plinth/server/internal/audit"
	"plinth/server/internal/calendar"
	"plinth/server/internal/categories"
	"plinth/server/internal/idempotency"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
	"plinth/server/internal/settings"
	"plinth/server/internal/transactions"
)

// maxPeriods bounds how many periods one request evaluates (weekly: about eleven years).
const maxPeriods = 600

var ErrVersion = apierr.New(http.StatusConflict, "version_conflict", "This budget changed since you loaded it. Reload and try again.")

type Budget struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	AccountID    *string   `json:"account_id"`
	CategoryID   *string   `json:"category_id"`
	Currency     string    `json:"currency"`
	Period       string    `json:"period"` // weekly | monthly | quarterly | yearly | custom
	StartDate    string    `json:"start_date"`
	EndDate      *string   `json:"end_date"`
	Rollover     bool      `json:"rollover"`
	AlertPercent *int      `json:"alert_percent"`
	Timezone     string    `json:"timezone"`
	Archived     bool      `json:"archived"`
	Version      int       `json:"version"`
	CreatedAt    time.Time `json:"created_at"`
}

// PeriodResult is one budget period evaluated against the ledger.
type PeriodResult struct {
	Start          string `json:"period_start"`
	End            string `json:"period_end"`
	Limit          int64  `json:"limit"`           // the limit that applied to this period
	CarriedOver    int64  `json:"carried_over"`    // unused amount brought in by rollover
	EffectiveLimit int64  `json:"effective_limit"` // limit + carried_over
	Spent          int64  `json:"spent"`           // net of linked refunds, never below zero
	Remaining      int64  `json:"remaining"`       // effective_limit - spent; negative when over
	PercentUsed    int    `json:"percent_used"`    // may exceed 100
	Over           bool   `json:"over"`
	OverBy         int64  `json:"over_by"`
}

// View is a budget with its current period.
type View struct {
	Budget
	PeriodResult
	Status               string `json:"status"` // ok | over | ended | not_started
	PeriodElapsedPercent int    `json:"period_elapsed_percent"`
	PaceDelta            int64  `json:"pace_delta"` // spent minus the even-pace amount; positive means ahead of pace
	AlertTriggered       bool   `json:"alert_triggered"`
}

type Input struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	AccountID    *string `json:"account_id"`
	CategoryID   *string `json:"category_id"`
	Currency     string  `json:"currency"`
	Period       string  `json:"period"`
	StartDate    string  `json:"start_date"`
	EndDate      *string `json:"end_date"`
	Limit        int64   `json:"limit"`
	Rollover     bool    `json:"rollover"`
	AlertPercent *int    `json:"alert_percent"`
	Timezone     string  `json:"timezone"`
}

type Service struct {
	db       *pgxpool.Pool
	settings *settings.Service
	now      func() time.Time
}

func NewService(db *pgxpool.Pool, st *settings.Service) *Service {
	return &Service{db: db, settings: st, now: time.Now}
}

const cols = `b.id::text, b.name, b.account_id::text, b.category_id::text, b.currency, b.period, to_char(b.start_date,'YYYY-MM-DD'),
	to_char(b.end_date,'YYYY-MM-DD'), b.rollover, b.alert_percent, b.timezone, b.archived_at IS NOT NULL, b.version, b.created_at`

func scan(row interface{ Scan(...any) error }) (Budget, error) {
	var b Budget
	err := row.Scan(&b.ID, &b.Name, &b.AccountID, &b.CategoryID, &b.Currency, &b.Period, &b.StartDate, &b.EndDate, &b.Rollover,
		&b.AlertPercent, &b.Timezone, &b.Archived, &b.Version, &b.CreatedAt)
	b.CreatedAt = b.CreatedAt.UTC()
	return b, err
}

func (s *Service) loc(tz string) *time.Location {
	l, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return l
}

func (s *Service) fetch(ctx context.Context, q dbx.Q, userID, id string) (Budget, error) {
	b, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM budgets b WHERE b.id=$1 AND b.user_id=$2`, id, userID))
	if dbx.IsNoRows(err) {
		return b, apierr.ErrNotFound
	}
	return b, err
}

type limitRow struct {
	from   time.Time
	amount int64
}

func (s *Service) limits(ctx context.Context, q dbx.Q, budgetID string) ([]limitRow, error) {
	rows, err := q.Query(ctx, `SELECT effective_from, amount FROM budget_limits WHERE budget_id=$1 ORDER BY effective_from`, budgetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []limitRow
	for rows.Next() {
		var l limitRow
		if err := rows.Scan(&l.from, &l.amount); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// limitFor returns the limit in force for a period starting at start: the row with the latest effective_from on
// or before it. Past periods therefore keep the limit they had when they began.
func limitFor(ls []limitRow, start time.Time) int64 {
	var amt int64
	for _, l := range ls {
		if l.from.After(start) {
			break
		}
		amt = l.amount
	}
	if amt == 0 && len(ls) > 0 {
		amt = ls[0].amount // a period before the first row (cannot normally happen): use the first limit
	}
	return amt
}

var bucketUnit = map[string]string{"weekly": "week", "monthly": "month", "quarterly": "quarter", "yearly": "year"}

// spentByPeriod returns net spend per period start between from and to (civil dates, inclusive). Eligible spend
// (docs/planning-rules.md): expenses that are pending or confirmed, not reconcile or settlement entries, counting only counted_amount when set, in the budget currency,
// inside the account and category scope (a category includes its subcategories), dated in the budget's zone.
// Transfers are never expenses. Each linked refund is subtracted in the period of the expense it refunds.
func (s *Service) spentByPeriod(ctx context.Context, q dbx.Q, userID string, b Budget, from, to time.Time) (map[string]int64, error) {
	bucket := `$8::date`
	if unit, ok := bucketUnit[b.Period]; ok {
		bucket = `date_trunc('` + unit + `', (t.occurred_at AT TIME ZONE $7))::date`
	}
	rows, err := q.Query(ctx, `WITH e AS (
			SELECT t.id, COALESCE(t.counted_amount, t.amount) AS amount, `+bucket+` AS b FROM transactions t
			WHERE t.user_id = $1 AND t.type = 'expense' AND t.status IN ('pending','confirmed') AND t.source NOT IN ('reconcile','settlement','topup') AND t.currency = $2
			  AND ($3::uuid IS NULL OR t.account_id = $3)
			  AND ($4::uuid IS NULL OR t.category_id = $4 OR t.category_id IN (SELECT id FROM categories WHERE parent_id = $4))
			  AND (t.occurred_at AT TIME ZONE $7)::date BETWEEN $5 AND $6 AND $8::date IS NOT NULL)
		SELECT to_char(b,'YYYY-MM-DD'), SUM(amount)::bigint FROM (
			SELECT b, amount FROM e
			UNION ALL
			SELECT e.b, -r.amount FROM e JOIN transactions r ON r.refund_of = e.id AND r.user_id = $1 AND r.status IN ('pending','confirmed')
		) x GROUP BY b`, userID, b.Currency, b.AccountID, b.CategoryID, from, to, b.Timezone, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		if v < 0 {
			v = 0
		}
		out[k] = v
	}
	return out, rows.Err()
}

// periodStarts lists the period start dates from the budget's first period up to the one containing `current`.
func periodStarts(b Budget, start, current time.Time) []time.Time {
	if b.Period == "custom" {
		return []time.Time{start}
	}
	var out []time.Time
	for p := calendar.PeriodStart(b.Period, start); !p.After(current); p = calendar.NextPeriodStart(b.Period, p) {
		out = append(out, p)
	}
	if len(out) > maxPeriods {
		out = out[len(out)-maxPeriods:]
	}
	return out
}

func percent(spent, limit int64) int {
	if limit <= 0 {
		return 0
	}
	return int(spent * 100 / limit)
}

// evaluate computes every period up to the one containing `today`. Rollover needs the whole chain, because each
// period's carry depends on the previous one; without rollover only the periods asked for are needed, but one
// grouped query covers them all either way.
func (s *Service) evaluate(ctx context.Context, q dbx.Q, userID string, b Budget, today time.Time) ([]PeriodResult, error) {
	start, _ := calendar.ParseDate(b.StartDate)
	current := today
	if today.Before(start) {
		current = start
	}
	if b.EndDate != nil {
		if end, _ := calendar.ParseDate(*b.EndDate); current.After(end) {
			current = end
		}
	}
	starts := periodStarts(b, start, current)
	ls, err := s.limits(ctx, q, b.ID)
	if err != nil {
		return nil, err
	}
	last := starts[len(starts)-1]
	to := calendar.PeriodEnd(b.Period, last)
	if b.Period == "custom" {
		to, _ = calendar.ParseDate(*b.EndDate)
	}
	spent, err := s.spentByPeriod(ctx, q, userID, b, starts[0], to)
	if err != nil {
		return nil, err
	}
	out := make([]PeriodResult, 0, len(starts))
	var carry int64
	for _, p := range starts {
		end := calendar.PeriodEnd(b.Period, p)
		if b.Period == "custom" {
			end, _ = calendar.ParseDate(*b.EndDate)
		}
		r := PeriodResult{Start: calendar.Format(p), End: calendar.Format(end), Limit: limitFor(ls, p), Spent: spent[calendar.Format(p)]}
		if b.Rollover {
			r.CarriedOver = carry
		}
		r.EffectiveLimit = r.Limit + r.CarriedOver
		r.Remaining = r.EffectiveLimit - r.Spent
		r.PercentUsed = percent(r.Spent, r.EffectiveLimit)
		if r.Remaining < 0 {
			r.Over, r.OverBy = true, -r.Remaining
		}
		if b.Rollover {
			carry = r.Remaining // only a positive leftover carries forward
			if carry < 0 {
				carry = 0
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Service) view(ctx context.Context, q dbx.Q, userID string, b Budget) (View, error) {
	loc := s.loc(b.Timezone)
	today := calendar.Today(s.now(), loc)
	periods, err := s.evaluate(ctx, q, userID, b, today)
	if err != nil {
		return View{}, err
	}
	cur := periods[len(periods)-1]
	v := View{Budget: b, PeriodResult: cur, Status: "ok"}
	ps, _ := calendar.ParseDate(cur.Start)
	pe, _ := calendar.ParseDate(cur.End)
	total := int64(pe.Sub(ps).Hours()/24) + 1
	elapsed := int64(today.Sub(ps).Hours()/24) + 1
	switch {
	case elapsed < 1:
		elapsed, v.Status = 0, "not_started"
	case elapsed > total:
		elapsed = total
		if b.Period == "custom" {
			v.Status = "ended"
		}
	}
	v.PeriodElapsedPercent = int(elapsed * 100 / total)
	v.PaceDelta = cur.Spent - cur.EffectiveLimit*elapsed/total
	if cur.Over && v.Status == "ok" {
		v.Status = "over"
	}
	v.AlertTriggered = b.AlertPercent != nil && cur.PercentUsed >= *b.AlertPercent
	return v, nil
}

func (s *Service) resolve(ctx context.Context, q dbx.Q, userID string, in Input, existing *Budget) (Input, time.Time, *time.Time, error) {
	f := map[string]string{}
	in.Name = strings.TrimSpace(in.Name)
	if n := len([]rune(in.Name)); n < 1 || n > 80 {
		f["name"] = "Enter a name of 1 to 80 characters."
	}
	if existing == nil {
		if _, ok := map[string]bool{"weekly": true, "monthly": true, "quarterly": true, "yearly": true, "custom": true}[in.Period]; !ok {
			f["period"] = "Choose weekly, monthly, quarterly, yearly or custom."
		}
	}
	if in.Limit <= 0 {
		f["limit"] = "Enter a limit greater than zero."
	} else if in.Limit > 1_000_000_000_000_000 {
		f["limit"] = "That amount is too large."
	}
	if in.AlertPercent != nil && (*in.AlertPercent < 1 || *in.AlertPercent > 200) {
		f["alert_percent"] = "Choose 1 to 200 percent."
	}
	st, err := s.settings.Get(ctx, userID)
	if err != nil {
		return in, time.Time{}, nil, err
	}
	if in.Timezone == "" {
		in.Timezone = st.Timezone
	}
	if l, err := time.LoadLocation(in.Timezone); err != nil || l == nil || in.Timezone == "Local" {
		f["timezone"] = "Choose a valid time zone, for example Asia/Kolkata."
	}
	var start time.Time
	var end *time.Time
	if in.StartDate == "" {
		start = calendar.Today(s.now(), s.loc(in.Timezone))
	} else if d, err := calendar.ParseDate(in.StartDate); err != nil {
		f["start_date"] = "Use a real date in the form YYYY-MM-DD."
	} else {
		start = d
	}
	if in.EndDate != nil && *in.EndDate != "" {
		e, err := calendar.ParseDate(*in.EndDate)
		switch {
		case err != nil:
			f["end_date"] = "Use a real date in the form YYYY-MM-DD."
		case e.Before(start):
			f["end_date"] = "The end date must be on or after the start date."
		default:
			end = &e
		}
	}
	if in.Period == "custom" && end == nil {
		f["end_date"] = "A custom budget needs an end date."
	}
	if in.Period != "custom" && end != nil && existing == nil {
		f["end_date"] = "Only a custom budget has an end date."
	}
	if in.Period == "custom" && in.Rollover {
		f["rollover"] = "A custom budget runs once, so there is nothing to roll over."
	}
	if len(f) > 0 {
		return in, start, end, apierr.Validation(f)
	}
	in.Currency = strings.ToUpper(in.Currency)
	if in.AccountID != nil && *in.AccountID != "" {
		aid := ids.Normalize(*in.AccountID)
		a, err := accounts.Fetch(ctx, q, userID, aid)
		if err != nil || !ids.Valid(aid) {
			return in, start, end, apierr.Validation(map[string]string{"account_id": "Choose one of your accounts."})
		}
		if in.Currency != "" && in.Currency != a.Currency {
			return in, start, end, apierr.Validation(map[string]string{"currency": "A budget covering an account uses that account's currency (" + a.Currency + ")."})
		}
		in.AccountID, in.Currency = &aid, a.Currency
	} else {
		in.AccountID = nil
		if in.Currency == "" {
			in.Currency = st.Currency
		}
	}
	if existing != nil && in.Currency != existing.Currency {
		return in, start, end, apierr.Validation(map[string]string{"account_id": "That account uses a different currency from this budget."})
	}
	if in.CategoryID != nil && *in.CategoryID != "" {
		cid := ids.Normalize(*in.CategoryID)
		c, err := categories.Visible(ctx, q, userID, cid)
		if err != nil || c.Kind != "expense" {
			return in, start, end, apierr.Validation(map[string]string{"category_id": "Choose one of your expense categories."})
		}
		in.CategoryID = &cid
	} else {
		in.CategoryID = nil
	}
	return in, start, end, nil
}

func (s *Service) Create(ctx context.Context, userID, key string, in Input) (View, error) {
	if err := idempotency.Check(key, false); err != nil {
		return View{}, err
	}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		return View{}, apierr.Validation(map[string]string{"id": "Identifier must be a UUID."})
	}
	if id == "" {
		id = ids.New()
	}
	var out View
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "budget.create", idempotency.Hash("budget.create", in))
		if err != nil {
			return err
		}
		if replay != nil {
			b, err := s.fetch(ctx, tx, userID, replay.ResourceID)
			if err != nil {
				return err
			}
			out, err = s.view(ctx, tx, userID, b)
			return err
		}
		r, start, end, err := s.resolve(ctx, tx, userID, in, nil)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO budgets (id, user_id, name, account_id, category_id, currency, period, start_date, end_date, rollover, alert_percent, timezone)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, id, userID, r.Name, r.AccountID, r.CategoryID, r.Currency, r.Period, start, end, r.Rollover, r.AlertPercent, r.Timezone); err != nil {
			if dbx.ConstraintName(err) == "budgets_pkey" {
				return transactions.ErrIDTaken
			}
			return err
		}
		first := start
		if r.Period != "custom" {
			first = calendar.PeriodStart(r.Period, start)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO budget_limits (budget_id, effective_from, amount) VALUES ($1,$2,$3)`, id, first, r.Limit); err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "budget.created", map[string]any{"budget_id": id}); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "budget", id); err != nil {
			return err
		}
		b, err := s.fetch(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		out, err = s.view(ctx, tx, userID, b)
		return err
	})
	return out, err
}

func (s *Service) Get(ctx context.Context, userID, id string) (View, error) {
	b, err := s.fetch(ctx, s.db, userID, id)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, s.db, userID, b)
}

func (s *Service) List(ctx context.Context, userID string, includeArchived bool) ([]View, error) {
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM budgets b WHERE b.user_id=$1 AND ($2 OR b.archived_at IS NULL) ORDER BY b.archived_at IS NOT NULL, lower(b.name), b.id`, userID, includeArchived)
	if err != nil {
		return nil, err
	}
	var bs []Budget
	for rows.Next() {
		b, err := scan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		bs = append(bs, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]View, 0, len(bs))
	for _, b := range bs {
		v, err := s.view(ctx, s.db, userID, b)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

type UpdateInput struct {
	Version      int     `json:"version"`
	Name         *string `json:"name"`
	AccountID    *string `json:"account_id"` // empty widens the scope to all accounts
	CategoryID   *string `json:"category_id"`
	Limit        *int64  `json:"limit"`
	Rollover     *bool   `json:"rollover"`
	AlertPercent *int    `json:"alert_percent"`
	ClearAlert   bool    `json:"clear_alert"`
	EndDate      *string `json:"end_date"` // custom budgets only
}

// Update edits a budget. A new limit takes effect from the start of the current period, so periods that already
// ended keep the limit they had (DESIGN.md section 11). Nothing here touches transactions.
func (s *Service) Update(ctx context.Context, userID, id string, in UpdateInput) (View, error) {
	if in.Version < 1 {
		return View{}, apierr.Validation(map[string]string{"version": "Send the version you loaded."})
	}
	var out View
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM budgets WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, userID); err != nil {
			return err
		}
		cur, err := s.fetch(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		if cur.Version != in.Version {
			return ErrVersion
		}
		ls, err := s.limits(ctx, tx, id)
		if err != nil {
			return err
		}
		today := calendar.Today(s.now(), s.loc(cur.Timezone))
		curStart, _ := calendar.ParseDate(cur.StartDate)
		if cur.Period != "custom" {
			ps := calendar.PeriodStart(cur.Period, today)
			if first := calendar.PeriodStart(cur.Period, curStart); ps.Before(first) {
				ps = first
			}
			curStart = ps
		}
		m := Input{Name: cur.Name, AccountID: cur.AccountID, CategoryID: cur.CategoryID, Currency: cur.Currency, Period: cur.Period, StartDate: cur.StartDate,
			EndDate: cur.EndDate, Limit: limitFor(ls, curStart), Rollover: cur.Rollover, AlertPercent: cur.AlertPercent, Timezone: cur.Timezone}
		if in.Name != nil {
			m.Name = *in.Name
		}
		if in.AccountID != nil {
			m.AccountID = in.AccountID
		}
		if in.CategoryID != nil {
			m.CategoryID = in.CategoryID
		}
		if in.Limit != nil {
			m.Limit = *in.Limit
		}
		if in.Rollover != nil {
			m.Rollover = *in.Rollover
		}
		if in.AlertPercent != nil {
			m.AlertPercent = in.AlertPercent
		}
		if in.ClearAlert {
			m.AlertPercent = nil
		}
		if in.EndDate != nil && cur.Period == "custom" {
			m.EndDate = in.EndDate
		}
		r, _, end, err := s.resolve(ctx, tx, userID, m, &cur)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE budgets SET name=$3, account_id=$4, category_id=$5, rollover=$6, alert_percent=$7, end_date=COALESCE($8, end_date),
			version=version+1, updated_at=now() WHERE id=$1 AND user_id=$2`, id, userID, r.Name, r.AccountID, r.CategoryID, r.Rollover, r.AlertPercent, end); err != nil {
			return err
		}
		if in.Limit != nil && *in.Limit != limitFor(ls, curStart) {
			if _, err := tx.Exec(ctx, `INSERT INTO budget_limits (budget_id, effective_from, amount) VALUES ($1,$2,$3)
				ON CONFLICT (budget_id, effective_from) DO UPDATE SET amount = EXCLUDED.amount`, id, curStart, *in.Limit); err != nil {
				return err
			}
		}
		if err := audit.Log(ctx, tx, userID, "budget.updated", map[string]any{"budget_id": id}); err != nil {
			return err
		}
		b, err := s.fetch(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		out, err = s.view(ctx, tx, userID, b)
		return err
	})
	return out, err
}

// SetArchived archives or restores a budget. Nothing is deleted.
func (s *Service) SetArchived(ctx context.Context, userID, id string, archive bool) (View, error) {
	tag, err := s.db.Exec(ctx, `UPDATE budgets SET archived_at = CASE WHEN $3 THEN COALESCE(archived_at, now()) ELSE NULL END, version=version+1, updated_at=now()
		WHERE id=$1 AND user_id=$2`, id, userID, archive)
	if err != nil {
		return View{}, err
	}
	if tag.RowsAffected() == 0 {
		return View{}, apierr.ErrNotFound
	}
	if err := audit.Log(ctx, s.db, userID, map[bool]string{true: "budget.archived", false: "budget.unarchived"}[archive], map[string]any{"budget_id": id}); err != nil {
		return View{}, err
	}
	return s.Get(ctx, userID, id)
}

// History returns the periods before the current one, newest first, each with the limit that applied to it.
func (s *Service) History(ctx context.Context, userID, id string, n int) ([]PeriodResult, error) {
	if n <= 0 || n > 60 {
		n = 6
	}
	b, err := s.fetch(ctx, s.db, userID, id)
	if err != nil {
		return nil, err
	}
	periods, err := s.evaluate(ctx, s.db, userID, b, calendar.Today(s.now(), s.loc(b.Timezone)))
	if err != nil {
		return nil, err
	}
	past := periods[:len(periods)-1]
	out := make([]PeriodResult, 0, n)
	for i := len(past) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, past[i])
	}
	return out, nil
}

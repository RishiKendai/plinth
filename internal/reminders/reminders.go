// Package reminders owns payment reminders (CLAUDE.md 3.14). A reminder never changes a balance. Marking one
// paid either links a transaction the user already recorded or creates one through the transactions module;
// the reminder code itself has no access to accounts or the ledger.
package reminders

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
	"plinth/server/internal/ledger"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
	"plinth/server/internal/settings"
	"plinth/server/internal/transactions"
)

var (
	ErrVersion = apierr.New(http.StatusConflict, "version_conflict", "This reminder changed since you loaded it. Reload and try again.")
	ErrClosed  = apierr.New(http.StatusConflict, "reminder_closed", "This reminder is already paid or deleted.")
)

type Reminder struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Notes         string     `json:"notes"`
	Amount        *int64     `json:"amount"`
	Currency      *string    `json:"currency"`
	AccountID     *string    `json:"account_id"`
	CategoryID    *string    `json:"category_id"`
	DueDate       string     `json:"due_date"`
	DueAt         time.Time  `json:"due_at"`
	TimeOfDay     string     `json:"time_of_day"`
	Timezone      string     `json:"timezone"`
	Frequency     *string    `json:"frequency"`
	Interval      int        `json:"interval"`
	Status        string     `json:"status"` // active | paid | cancelled
	State         string     `json:"state"`  // upcoming | due | snoozed | paid | cancelled
	SnoozedUntil  *time.Time `json:"snoozed_until"`
	LastPaidAt    *time.Time `json:"last_paid_at"`
	TransactionID *string    `json:"transaction_id"`
	Version       int        `json:"version"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type Input struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Notes      string  `json:"notes"`
	Amount     *int64  `json:"amount"`
	AccountID  *string `json:"account_id"`
	CategoryID *string `json:"category_id"`
	DueDate    string  `json:"due_date"`
	TimeOfDay  string  `json:"time_of_day"`
	Timezone   string  `json:"timezone"`
	Frequency  *string `json:"frequency"`
	Interval   int     `json:"interval"`
}

type Service struct {
	db       *pgxpool.Pool
	settings *settings.Service
	txs      *transactions.Service
	now      func() time.Time
}

func NewService(db *pgxpool.Pool, st *settings.Service, txs *transactions.Service) *Service {
	return &Service{db: db, settings: st, txs: txs, now: time.Now}
}

const stateExpr = `(CASE r.status WHEN 'paid' THEN 'paid' WHEN 'cancelled' THEN 'cancelled' ELSE
	CASE WHEN r.snoozed_until IS NOT NULL AND r.snoozed_until > now() THEN 'snoozed'
	     WHEN r.due_at <= now() THEN 'due' ELSE 'upcoming' END END)`

// due_at is computed in SQL from the civil date, local time and zone, so list filters and the response agree.
const cols = `r.id::text, r.title, r.notes, r.amount, r.currency, r.account_id::text, r.category_id::text, to_char(r.due_date,'YYYY-MM-DD'),
	r.due_at, to_char(r.time_of_day,'HH24:MI'), r.timezone, r.frequency, r.interval, r.status, ` + stateExpr + `,
	r.snoozed_until, r.last_paid_at, r.transaction_id::text, r.version, r.created_at, r.updated_at`

// A view that adds due_at; the base table stores date + time + zone so a zone rule change cannot strand a stale instant.
const from = `(SELECT *, ((due_date + time_of_day) AT TIME ZONE timezone) AS due_at FROM reminders) r`

func scan(row interface{ Scan(...any) error }) (Reminder, error) {
	var x Reminder
	err := row.Scan(&x.ID, &x.Title, &x.Notes, &x.Amount, &x.Currency, &x.AccountID, &x.CategoryID, &x.DueDate, &x.DueAt, &x.TimeOfDay, &x.Timezone,
		&x.Frequency, &x.Interval, &x.Status, &x.State, &x.SnoozedUntil, &x.LastPaidAt, &x.TransactionID, &x.Version, &x.CreatedAt, &x.UpdatedAt)
	x.DueAt, x.CreatedAt, x.UpdatedAt = x.DueAt.UTC(), x.CreatedAt.UTC(), x.UpdatedAt.UTC()
	return x, err
}

func load(ctx context.Context, q dbx.Q, userID, id string) (Reminder, error) {
	x, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM `+from+` WHERE r.id=$1 AND r.user_id=$2`, id, userID))
	if dbx.IsNoRows(err) {
		return x, apierr.ErrNotFound
	}
	return x, err
}

func (s *Service) Get(ctx context.Context, userID, id string) (Reminder, error) {
	return load(ctx, s.db, userID, id)
}

// List returns reminders. state: due, upcoming, snoozed, paid, cancelled, or "" for everything still active.
func (s *Service) List(ctx context.Context, userID, state string) ([]Reminder, error) {
	switch state {
	case "", "due", "upcoming", "snoozed", "paid", "cancelled":
	default:
		return nil, apierr.Validation(map[string]string{"state": "Choose due, upcoming, snoozed, paid or cancelled."})
	}
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM `+from+` WHERE r.user_id=$1
		AND (($2 = '' AND r.status = 'active') OR ($2 <> '' AND `+stateExpr+` = $2))
		ORDER BY r.due_at, r.id`, userID, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reminder{}
	for rows.Next() {
		x, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

type fields struct {
	title, notes, tod, tz string
	amount                *int64
	currency              *string
	account, category     *string
	due                   time.Time
	freq                  *string
	interval              int
}

func (s *Service) resolve(ctx context.Context, q dbx.Q, userID string, in Input) (fields, error) {
	f := map[string]string{}
	r := fields{title: strings.TrimSpace(in.Title), notes: strings.TrimSpace(in.Notes), tod: in.TimeOfDay, tz: in.Timezone, amount: in.Amount, interval: in.Interval, freq: in.Frequency}
	if n := len([]rune(r.title)); n < 1 || n > 120 {
		f["title"] = "Enter a title of 1 to 120 characters."
	}
	if len([]rune(r.notes)) > 2000 {
		f["notes"] = "Use 2,000 characters or fewer."
	}
	if r.tod == "" {
		r.tod = "09:00"
	}
	if _, err := time.Parse("15:04", r.tod); err != nil {
		f["time_of_day"] = "Use a time like 09:00."
	}
	st, err := s.settings.Get(ctx, userID)
	if err != nil {
		return r, err
	}
	if r.tz == "" {
		r.tz = st.Timezone
	}
	if l, err := time.LoadLocation(r.tz); err != nil || l == nil || r.tz == "Local" {
		f["timezone"] = "Choose a valid time zone, for example Asia/Kolkata."
	}
	d, err := calendar.ParseDate(in.DueDate)
	if err != nil {
		f["due_date"] = "Use a real date in the form YYYY-MM-DD."
	}
	r.due = d
	if r.freq != nil && *r.freq == "" {
		r.freq = nil
	}
	if r.freq != nil && !calendar.ValidFreq(*r.freq) {
		f["frequency"] = "Choose daily, weekly, monthly, quarterly or yearly."
	}
	if r.interval == 0 {
		r.interval = 1
	}
	if r.interval < 1 || r.interval > 365 {
		f["interval"] = "Repeat every 1 to 365."
	}
	if in.Amount != nil && (*in.Amount <= 0 || *in.Amount > 1_000_000_000_000_000) {
		f["amount"] = "Enter an amount greater than zero."
	}
	if len(f) > 0 {
		return r, apierr.Validation(f)
	}
	cur := st.Currency
	if in.AccountID != nil && *in.AccountID != "" {
		aid := ids.Normalize(*in.AccountID)
		a, err := accounts.Fetch(ctx, q, userID, aid)
		if err != nil || !ids.Valid(aid) {
			return r, apierr.Validation(map[string]string{"account_id": "Choose one of your accounts."})
		}
		if a.ArchivedAt != nil {
			return r, apierr.Validation(map[string]string{"account_id": "That account is archived."})
		}
		r.account, cur = &aid, a.Currency
	}
	if in.CategoryID != nil && *in.CategoryID != "" {
		cid := ids.Normalize(*in.CategoryID)
		c, err := categories.Visible(ctx, q, userID, cid)
		if err != nil || c.Kind != "expense" {
			return r, apierr.Validation(map[string]string{"category_id": "Choose one of your expense categories."})
		}
		r.category = &cid
	}
	if r.amount != nil {
		r.currency = &cur
	}
	return r, nil
}

func (s *Service) Create(ctx context.Context, userID, key string, in Input) (Reminder, error) {
	if err := idempotency.Check(key, false); err != nil {
		return Reminder{}, err
	}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		return Reminder{}, apierr.Validation(map[string]string{"id": "Identifier must be a UUID."})
	}
	if id == "" {
		id = ids.New()
	}
	var out Reminder
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "reminder.create", idempotency.Hash("reminder.create", in))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = load(ctx, tx, userID, replay.ResourceID)
			return err
		}
		r, err := s.resolve(ctx, tx, userID, in)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO reminders (id, user_id, title, notes, amount, currency, account_id, category_id, timezone, time_of_day,
			anchor_date, due_date, frequency, interval) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,$12,$13)`,
			id, userID, r.title, r.notes, r.amount, r.currency, r.account, r.category, r.tz, r.tod, r.due, r.freq, r.interval)
		if err != nil {
			if dbx.ConstraintName(err) == "reminders_pkey" {
				return transactions.ErrIDTaken
			}
			return err
		}
		if err := audit.Log(ctx, tx, userID, "reminder.created", map[string]any{"reminder_id": id}); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "reminder", id); err != nil {
			return err
		}
		out, err = load(ctx, tx, userID, id)
		return err
	})
	return out, err
}

type UpdateInput struct {
	Version     int     `json:"version"`
	Title       *string `json:"title"`
	Notes       *string `json:"notes"`
	Amount      *int64  `json:"amount"`
	ClearAmount bool    `json:"clear_amount"`
	AccountID   *string `json:"account_id"`
	CategoryID  *string `json:"category_id"`
	DueDate     *string `json:"due_date"`
	TimeOfDay   *string `json:"time_of_day"`
	Timezone    *string `json:"timezone"`
	Frequency   *string `json:"frequency"` // empty makes it one-off
	Interval    *int    `json:"interval"`
}

func (s *Service) Update(ctx context.Context, userID, id string, in UpdateInput) (Reminder, error) {
	if in.Version < 1 {
		return Reminder{}, apierr.Validation(map[string]string{"version": "Send the version you loaded."})
	}
	var out Reminder
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// Lock the base row (the read view is a subselect and cannot be locked directly), then read it.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM reminders WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, userID); err != nil {
			return err
		}
		cur, err := load(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		if cur.Version != in.Version {
			return ErrVersion
		}
		if cur.Status != "active" {
			return ErrClosed
		}
		m := Input{Title: cur.Title, Notes: cur.Notes, Amount: cur.Amount, AccountID: cur.AccountID, CategoryID: cur.CategoryID, DueDate: cur.DueDate,
			TimeOfDay: cur.TimeOfDay, Timezone: cur.Timezone, Frequency: cur.Frequency, Interval: cur.Interval}
		if in.Title != nil {
			m.Title = *in.Title
		}
		if in.Notes != nil {
			m.Notes = *in.Notes
		}
		if in.Amount != nil {
			m.Amount = in.Amount
		}
		if in.ClearAmount {
			m.Amount = nil
		}
		if in.AccountID != nil {
			m.AccountID = in.AccountID
		}
		if in.CategoryID != nil {
			m.CategoryID = in.CategoryID
		}
		if in.DueDate != nil {
			m.DueDate = *in.DueDate
		}
		if in.TimeOfDay != nil {
			m.TimeOfDay = *in.TimeOfDay
		}
		if in.Timezone != nil {
			m.Timezone = *in.Timezone
		}
		if in.Frequency != nil {
			m.Frequency = in.Frequency
		}
		if in.Interval != nil {
			m.Interval = *in.Interval
		}
		r, err := s.resolve(ctx, tx, userID, m)
		if err != nil {
			return err
		}
		// Rescheduling re-anchors repeats on the new due date.
		if _, err := tx.Exec(ctx, `UPDATE reminders SET title=$3, notes=$4, amount=$5, currency=$6, account_id=$7, category_id=$8, timezone=$9,
			time_of_day=$10, due_date=$11, anchor_date=CASE WHEN due_date <> $11 THEN $11 ELSE anchor_date END, frequency=$12, interval=$13,
			snoozed_until=CASE WHEN due_date <> $11 THEN NULL ELSE snoozed_until END, version=version+1, updated_at=now() WHERE id=$1 AND user_id=$2`,
			id, userID, r.title, r.notes, r.amount, r.currency, r.account, r.category, r.tz, r.tod, r.due, r.freq, r.interval); err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "reminder.updated", map[string]any{"reminder_id": id}); err != nil {
			return err
		}
		out, err = load(ctx, tx, userID, id)
		return err
	})
	return out, err
}

// Delete cancels a reminder. Only the reminder is affected; any transaction it was paid with is untouched.
func (s *Service) Delete(ctx context.Context, userID, id string) error {
	tag, err := s.db.Exec(ctx, `UPDATE reminders SET status='cancelled', version=version+1, updated_at=now() WHERE id=$1 AND user_id=$2 AND status='active'`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := load(ctx, s.db, userID, id); err != nil {
			return err
		}
		return ErrClosed
	}
	return audit.Log(ctx, s.db, userID, "reminder.deleted", map[string]any{"reminder_id": id})
}

// Snooze hides a due reminder until a later time. The due date does not change.
func (s *Service) Snooze(ctx context.Context, userID, id string, until time.Time) (Reminder, error) {
	now := s.now()
	if !until.After(now) || until.After(now.AddDate(1, 0, 0)) {
		return Reminder{}, apierr.Validation(map[string]string{"until": "Choose a time in the future, within a year."})
	}
	tag, err := s.db.Exec(ctx, `UPDATE reminders SET snoozed_until=$3, version=version+1, updated_at=now() WHERE id=$1 AND user_id=$2 AND status='active'`, id, userID, until.UTC())
	if err != nil {
		return Reminder{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := load(ctx, s.db, userID, id); err != nil {
			return Reminder{}, err
		}
		return Reminder{}, ErrClosed
	}
	return load(ctx, s.db, userID, id)
}

// PayInput either links an existing transaction or describes the one to create.
type PayInput struct {
	TransactionID *string    `json:"transaction_id"`
	AccountID     *string    `json:"account_id"`
	Amount        *int64     `json:"amount"`
	OccurredAt    *time.Time `json:"occurred_at"`
}

// Pay marks a reminder paid. It must end with a real transaction: the user links one they already recorded, or
// a new expense is created through the transactions module (so balances move by the ledger rules and nowhere
// else). A repeating reminder moves to its next due date; a one-off becomes paid.
func (s *Service) Pay(ctx context.Context, userID, key, id string, in PayInput) (Reminder, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Reminder{}, err
	}
	var out Reminder
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "reminder.pay", idempotency.Hash("reminder.pay", []any{id, in}))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = load(ctx, tx, userID, replay.ResourceID)
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM reminders WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, userID); err != nil {
			return err
		}
		cur, err := load(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		if cur.Status != "active" {
			return ErrClosed
		}
		var txID string
		if in.TransactionID != nil {
			txID = ids.Normalize(*in.TransactionID)
			var typ, status string
			err := tx.QueryRow(ctx, `SELECT type, status FROM transactions WHERE id=$1 AND user_id=$2`, txID, userID).Scan(&typ, &status)
			if dbx.IsNoRows(err) || !ids.Valid(txID) || (err == nil && (typ != "expense" || !ledger.Counting(status))) {
				return apierr.Validation(map[string]string{"transaction_id": "Choose a recorded expense."})
			}
			if err != nil {
				return err
			}
		} else {
			acct := cur.AccountID
			if in.AccountID != nil {
				a := ids.Normalize(*in.AccountID)
				acct = &a
			}
			amount := cur.Amount
			if in.Amount != nil {
				amount = in.Amount
			}
			f := map[string]string{}
			if acct == nil {
				f["account_id"] = "Choose the account you paid from."
			}
			if amount == nil {
				f["amount"] = "Enter the amount you paid."
			}
			if len(f) > 0 {
				return apierr.Validation(f)
			}
			n := transactions.NewTx{AccountID: *acct, Type: "expense", Amount: *amount, Title: cur.Title, CategoryID: cur.CategoryID, Notes: cur.Notes,
				Status: "confirmed", OccurredAt: s.now().UTC(), Timezone: cur.Timezone, Source: "manual",
				ExternalRef: "reminder:" + id + ":" + cur.DueDate}
			if in.OccurredAt != nil {
				n.OccurredAt = in.OccurredAt.UTC()
			}
			if txID, err = s.txs.CreateInTx(ctx, tx, userID, n); err != nil {
				return err
			}
		}
		if cur.Frequency == nil {
			_, err = tx.Exec(ctx, `UPDATE reminders SET status='paid', last_paid_at=now(), transaction_id=$2, snoozed_until=NULL, version=version+1, updated_at=now() WHERE id=$1`, id, txID)
		} else {
			var anchor, due time.Time
			if err := tx.QueryRow(ctx, `SELECT anchor_date, due_date FROM reminders WHERE id=$1`, id).Scan(&anchor, &due); err != nil {
				return err
			}
			next := calendar.NextAfter(anchor, calendar.Freq(*cur.Frequency), cur.Interval, due)
			_, err = tx.Exec(ctx, `UPDATE reminders SET due_date=$3, last_paid_at=now(), transaction_id=$2, snoozed_until=NULL, version=version+1, updated_at=now() WHERE id=$1`, id, txID, next)
		}
		if err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "reminder.paid", map[string]any{"reminder_id": id, "transaction_id": txID}); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "reminder", id); err != nil {
			return err
		}
		out, err = load(ctx, tx, userID, id)
		return err
	})
	return out, err
}

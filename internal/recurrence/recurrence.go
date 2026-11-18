// Package recurrence owns recurring schedules and their occurrences (CLAUDE.md 3.8).
//
// A schedule is a plan. Each occurrence is an *expectation*: it never changes a balance. Money moves only when
// the user confirms an occurrence, which records a real transaction through the transactions module with a
// source-event reference (recurrence:<occurrence id>), so one occurrence can produce at most one transaction.
//
// Generation is idempotent: occurrences are materialised up to a horizon, a unique (schedule, due date) index
// makes a repeat run a no-op, and a stored cursor (generated_through) means a date is never revisited.
package recurrence

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

const (
	// HorizonDays is how far ahead occurrences are generated. A harmless implementation detail, not a product rule.
	HorizonDays = 60
	// maxPerRun bounds work per schedule per run, so a schedule backdated by years cannot flood the table.
	maxPerRun = 400
	// maxBackdateDays limits how far back a new schedule may start, since each past date becomes an occurrence to review.
	maxBackdateDays = 366
)

var (
	ErrVersion = apierr.New(http.StatusConflict, "version_conflict", "This schedule changed since you loaded it. Reload and try again.")
	ErrClosed  = apierr.New(http.StatusConflict, "occurrence_closed", "That occurrence is already confirmed or skipped.")
	ErrState   = apierr.New(http.StatusConflict, "invalid_state", "That schedule is not in a state that allows this.")
)

type Schedule struct {
	ID                 string    `json:"id"`
	AccountID          string    `json:"account_id"`
	Type               string    `json:"type"`
	Title              string    `json:"title"`
	Amount             int64     `json:"amount"`
	Currency           string    `json:"currency"`
	CategoryID         *string   `json:"category_id"`
	PaymentMethod      string    `json:"payment_method"`
	PayeeName          string    `json:"payee_name"`
	Notes              string    `json:"notes"`
	Frequency          string    `json:"frequency"`
	Interval           int       `json:"interval"`
	StartDate          string    `json:"start_date"`
	EndDate            *string   `json:"end_date"`
	Timezone           string    `json:"timezone"`
	TimeOfDay          string    `json:"time_of_day"`
	ReminderDaysBefore *int      `json:"reminder_days_before"`
	Status             string    `json:"status"`
	NextDueDate        *string   `json:"next_due_date"`
	Version            int       `json:"version"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type Occurrence struct {
	ID                string     `json:"id"`
	ScheduleID        string     `json:"schedule_id"`
	Title             string     `json:"title"`
	Type              string     `json:"type"`
	AccountID         string     `json:"account_id"`
	DueDate           string     `json:"due_date"`
	DueAt             time.Time  `json:"due_at"`
	ExpectedAmount    int64      `json:"expected_amount"`
	Currency          string     `json:"currency"`
	Status            string     `json:"status"` // expected | confirmed | skipped
	State             string     `json:"state"`  // upcoming | overdue | confirmed | skipped
	TransactionID     *string    `json:"transaction_id"`
	TransactionStatus *string    `json:"transaction_status"`
	ActualAmount      *int64     `json:"actual_amount"`
	Difference        *int64     `json:"difference"` // actual minus expected, once confirmed
	ResolvedAt        *time.Time `json:"resolved_at"`
}

type Input struct {
	ID                 string  `json:"id"`
	AccountID          string  `json:"account_id"`
	Type               string  `json:"type"`
	Title              string  `json:"title"`
	Amount             int64   `json:"amount"`
	CategoryID         *string `json:"category_id"`
	PaymentMethod      string  `json:"payment_method"`
	PayeeName          string  `json:"payee_name"`
	Notes              string  `json:"notes"`
	Frequency          string  `json:"frequency"`
	Interval           int     `json:"interval"`
	StartDate          string  `json:"start_date"`
	EndDate            *string `json:"end_date"`
	Timezone           string  `json:"timezone"`
	TimeOfDay          string  `json:"time_of_day"`
	ReminderDaysBefore *int    `json:"reminder_days_before"`
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

const schedCols = `s.id::text, s.account_id::text, s.type, s.title, s.amount, s.currency, s.category_id::text, s.payment_method, s.payee_name,
	s.notes, s.frequency, s.interval, to_char(s.start_date,'YYYY-MM-DD'), to_char(s.end_date,'YYYY-MM-DD'), s.timezone,
	to_char(s.time_of_day,'HH24:MI'), s.reminder_days_before, s.status,
	(SELECT to_char(min(o.due_date),'YYYY-MM-DD') FROM recurring_occurrences o WHERE o.schedule_id = s.id AND o.status = 'expected' AND o.due_at >= now()),
	s.version, s.created_at, s.updated_at`

func scanSched(row interface{ Scan(...any) error }) (Schedule, error) {
	var s Schedule
	err := row.Scan(&s.ID, &s.AccountID, &s.Type, &s.Title, &s.Amount, &s.Currency, &s.CategoryID, &s.PaymentMethod, &s.PayeeName,
		&s.Notes, &s.Frequency, &s.Interval, &s.StartDate, &s.EndDate, &s.Timezone, &s.TimeOfDay, &s.ReminderDaysBefore, &s.Status,
		&s.NextDueDate, &s.Version, &s.CreatedAt, &s.UpdatedAt)
	s.CreatedAt, s.UpdatedAt = s.CreatedAt.UTC(), s.UpdatedAt.UTC()
	return s, err
}

const occCols = `o.id::text, o.schedule_id::text, s.title, s.type, s.account_id::text, to_char(o.due_date,'YYYY-MM-DD'), o.due_at, o.expected_amount,
	o.currency, o.status,
	CASE o.status WHEN 'confirmed' THEN 'confirmed' WHEN 'skipped' THEN 'skipped' WHEN 'expected' THEN CASE WHEN o.due_at < now() THEN 'overdue' ELSE 'upcoming' END END,
	o.transaction_id::text, t.status, t.amount, t.amount - o.expected_amount, o.resolved_at`

const occFrom = `FROM recurring_occurrences o JOIN recurring_schedules s ON s.id = o.schedule_id LEFT JOIN transactions t ON t.id = o.transaction_id`

func scanOcc(row interface{ Scan(...any) error }) (Occurrence, error) {
	var o Occurrence
	err := row.Scan(&o.ID, &o.ScheduleID, &o.Title, &o.Type, &o.AccountID, &o.DueDate, &o.DueAt, &o.ExpectedAmount, &o.Currency, &o.Status,
		&o.State, &o.TransactionID, &o.TransactionStatus, &o.ActualAmount, &o.Difference, &o.ResolvedAt)
	o.DueAt = o.DueAt.UTC()
	return o, err
}

func (s *Service) loc(tz string) *time.Location {
	l, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return l
}

type resolved struct {
	in       Input
	start    time.Time
	end      *time.Time
	tod      string
	currency string
}

// resolve validates an input against the user's accounts and categories. It fills defaults.
func (s *Service) resolve(ctx context.Context, q dbx.Q, userID string, in Input, partialStart *time.Time) (resolved, error) {
	f := map[string]string{}
	r := resolved{in: in}
	r.in.Title = strings.TrimSpace(in.Title)
	if n := len([]rune(r.in.Title)); n < 1 || n > 120 {
		f["title"] = "Enter a title of 1 to 120 characters."
	}
	if in.Type != "expense" && in.Type != "income" {
		f["type"] = "Choose expense or income."
	}
	if in.Amount <= 0 {
		f["amount"] = "Enter an amount greater than zero."
	} else if in.Amount > 1_000_000_000_000_000 {
		f["amount"] = "That amount is too large."
	}
	if !calendar.ValidFreq(in.Frequency) {
		f["frequency"] = "Choose daily, weekly, monthly, quarterly or yearly."
	}
	if r.in.Interval == 0 {
		r.in.Interval = 1
	}
	if r.in.Interval < 1 || r.in.Interval > 365 {
		f["interval"] = "Repeat every 1 to 365."
	}
	if len([]rune(in.Notes)) > 2000 {
		f["notes"] = "Use 2,000 characters or fewer."
	}
	if len([]rune(in.PayeeName)) > 120 {
		f["payee_name"] = "Use 120 characters or fewer."
	}
	if in.ReminderDaysBefore != nil && (*in.ReminderDaysBefore < 0 || *in.ReminderDaysBefore > 30) {
		f["reminder_days_before"] = "Choose 0 to 30 days."
	}
	r.tod = in.TimeOfDay
	if r.tod == "" {
		r.tod = "09:00"
	}
	if _, err := time.Parse("15:04", r.tod); err != nil {
		f["time_of_day"] = "Use a time like 09:00."
	}
	if in.Timezone == "" {
		st, err := s.settings.Get(ctx, userID)
		if err != nil {
			return r, err
		}
		r.in.Timezone = st.Timezone
	}
	if l, err := time.LoadLocation(r.in.Timezone); err != nil || r.in.Timezone == "Local" || l == nil {
		f["timezone"] = "Choose a valid time zone, for example Asia/Kolkata."
	}
	if partialStart != nil {
		r.start = *partialStart
	} else if st, err := calendar.ParseDate(in.StartDate); err != nil {
		f["start_date"] = "Use a real date in the form YYYY-MM-DD."
	} else {
		r.start = st
	}
	if in.EndDate != nil && *in.EndDate != "" {
		e, err := calendar.ParseDate(*in.EndDate)
		switch {
		case err != nil:
			f["end_date"] = "Use a real date in the form YYYY-MM-DD."
		case !r.start.IsZero() && e.Before(r.start):
			f["end_date"] = "The end date must be on or after the start date."
		default:
			r.end = &e
		}
	}
	if len(f) > 0 {
		return r, apierr.Validation(f)
	}
	aid := ids.Normalize(in.AccountID)
	if !ids.Valid(aid) {
		return r, apierr.Validation(map[string]string{"account_id": "Choose one of your accounts."})
	}
	acct, err := accounts.Fetch(ctx, q, userID, aid)
	if err != nil {
		return r, apierr.Validation(map[string]string{"account_id": "Choose one of your accounts."})
	}
	if acct.ArchivedAt != nil {
		return r, apierr.Validation(map[string]string{"account_id": "That account is archived."})
	}
	r.in.AccountID, r.currency = aid, acct.Currency
	if in.CategoryID != nil && *in.CategoryID != "" {
		cid := ids.Normalize(*in.CategoryID)
		c, err := categories.Visible(ctx, q, userID, cid)
		if err != nil || c.Kind != in.Type {
			return r, apierr.Validation(map[string]string{"category_id": "Choose one of your " + in.Type + " categories."})
		}
		r.in.CategoryID = &cid
	} else {
		r.in.CategoryID = nil
	}
	return r, nil
}

// Create makes a schedule and generates its first occurrences. A backdated start produces past occurrences that
// the user reviews as overdue; they are expectations, not transactions.
func (s *Service) Create(ctx context.Context, userID, key string, in Input) (Schedule, error) {
	if err := idempotency.Check(key, false); err != nil {
		return Schedule{}, err
	}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		return Schedule{}, apierr.Validation(map[string]string{"id": "Identifier must be a UUID."})
	}
	if id == "" {
		id = ids.New()
	}
	var out Schedule
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "recurrence.create", idempotency.Hash("recurrence.create", in))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = scanSched(tx.QueryRow(ctx, `SELECT `+schedCols+` FROM recurring_schedules s WHERE s.id=$1 AND s.user_id=$2`, replay.ResourceID, userID))
			return err
		}
		r, err := s.resolve(ctx, tx, userID, in, nil)
		if err != nil {
			return err
		}
		today := calendar.Today(s.now(), s.loc(r.in.Timezone))
		if r.start.Before(today.AddDate(0, 0, -maxBackdateDays)) {
			return apierr.Validation(map[string]string{"start_date": "A schedule can start at most a year ago."})
		}
		_, err = tx.Exec(ctx, `INSERT INTO recurring_schedules (id, user_id, account_id, type, title, amount, currency, category_id, payment_method,
			payee_name, notes, frequency, interval, start_date, end_date, timezone, time_of_day, reminder_days_before, generated_through)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
			id, userID, r.in.AccountID, r.in.Type, r.in.Title, r.in.Amount, r.currency, r.in.CategoryID, r.in.PaymentMethod,
			r.in.PayeeName, r.in.Notes, r.in.Frequency, r.in.Interval, r.start, r.end, r.in.Timezone, r.tod, r.in.ReminderDaysBefore,
			r.start.AddDate(0, 0, -1))
		if err != nil {
			if dbx.ConstraintName(err) == "recurring_schedules_pkey" {
				return transactions.ErrIDTaken
			}
			return err
		}
		if _, err := s.generate(ctx, tx, id, s.now()); err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "recurrence.created", map[string]any{"schedule_id": id}); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "recurrence", id); err != nil {
			return err
		}
		out, err = scanSched(tx.QueryRow(ctx, `SELECT `+schedCols+` FROM recurring_schedules s WHERE s.id=$1`, id))
		return err
	})
	return out, err
}

type row struct {
	id, userID, freq, tz, tod, status, currency string
	interval                                    int
	amount                                      int64
	start, through                              time.Time
	end                                         *time.Time
}

// generate materialises the schedule's occurrences up to the horizon, resuming from its cursor. It is safe to
// call any number of times (unique index plus cursor). It returns how many rows were inserted.
func (s *Service) generate(ctx context.Context, tx pgx.Tx, scheduleID string, now time.Time) (int, error) {
	var r row
	err := tx.QueryRow(ctx, `SELECT id::text, user_id::text, frequency, timezone, to_char(time_of_day,'HH24:MI'), status, currency, interval, amount,
		start_date, generated_through, end_date FROM recurring_schedules WHERE id=$1 FOR UPDATE`, scheduleID).
		Scan(&r.id, &r.userID, &r.freq, &r.tz, &r.tod, &r.status, &r.currency, &r.interval, &r.amount, &r.start, &r.through, &r.end)
	if err != nil {
		return 0, err
	}
	if r.status != "active" {
		return 0, nil
	}
	loc := s.loc(r.tz)
	horizon := calendar.Today(now, loc).AddDate(0, 0, HorizonDays)
	dates := calendar.Between(r.start, calendar.Freq(r.freq), r.interval, r.through, horizon, r.end, maxPerRun)
	inserted := 0
	for _, d := range dates {
		due, err := calendar.DueAt(d, r.tod, loc)
		if err != nil {
			return inserted, err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO recurring_occurrences (schedule_id, user_id, due_date, due_at, expected_amount, currency)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (schedule_id, due_date) DO NOTHING`, r.id, r.userID, d, due, r.amount, r.currency)
		if err != nil {
			return inserted, err
		}
		inserted += int(tag.RowsAffected())
	}
	newThrough := horizon
	if r.end != nil && r.end.Before(newThrough) {
		newThrough = *r.end
	}
	if len(dates) == maxPerRun {
		newThrough = dates[len(dates)-1] // more to do on the next run
	}
	if newThrough.After(r.through) {
		_, err = tx.Exec(ctx, `UPDATE recurring_schedules SET generated_through=$2 WHERE id=$1`, r.id, newThrough)
	}
	return inserted, err
}

// GenerateAll advances every active schedule that is behind the horizon. The worker calls it on a timer. Each
// schedule is handled in its own short transaction with SKIP LOCKED, so several workers can run safely and a
// failure on one schedule does not block the rest. It returns the number of occurrences created.
func (s *Service) GenerateAll(ctx context.Context, batch int) (int, error) {
	total := 0
	for {
		n, done, err := s.generateBatch(ctx, batch)
		total += n
		if err != nil {
			return total, err
		}
		if done {
			return total, nil
		}
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}

func (s *Service) generateBatch(ctx context.Context, batch int) (created int, done bool, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text FROM recurring_schedules WHERE status='active'
			AND (generated_through < (now() AT TIME ZONE timezone)::date + $1::int AND (end_date IS NULL OR generated_through < end_date))
			ORDER BY generated_through LIMIT $2 FOR UPDATE SKIP LOCKED`, HorizonDays, batch)
		if err != nil {
			return err
		}
		var idsToDo []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			idsToDo = append(idsToDo, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		done = len(idsToDo) < batch
		for _, id := range idsToDo {
			n, err := s.generate(ctx, tx, id, s.now())
			if err != nil {
				return err
			}
			created += n
		}
		return nil
	})
	return
}

func (s *Service) Get(ctx context.Context, userID, id string) (Schedule, error) {
	out, err := scanSched(s.db.QueryRow(ctx, `SELECT `+schedCols+` FROM recurring_schedules s WHERE s.id=$1 AND s.user_id=$2`, id, userID))
	if dbx.IsNoRows(err) {
		return out, apierr.ErrNotFound
	}
	return out, err
}

func (s *Service) List(ctx context.Context, userID, status string) ([]Schedule, error) {
	rows, err := s.db.Query(ctx, `SELECT `+schedCols+` FROM recurring_schedules s WHERE s.user_id=$1 AND ($2 = '' OR s.status = $2)
		ORDER BY s.status = 'cancelled', lower(s.title), s.id`, userID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		x, err := scanSched(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

type UpdateInput struct {
	Version            int     `json:"version"`
	AccountID          *string `json:"account_id"`
	Title              *string `json:"title"`
	Amount             *int64  `json:"amount"`
	CategoryID         *string `json:"category_id"` // empty clears
	PaymentMethod      *string `json:"payment_method"`
	PayeeName          *string `json:"payee_name"`
	Notes              *string `json:"notes"`
	Frequency          *string `json:"frequency"`
	Interval           *int    `json:"interval"`
	EndDate            *string `json:"end_date"` // empty clears
	Timezone           *string `json:"timezone"`
	TimeOfDay          *string `json:"time_of_day"`
	ReminderDaysBefore *int    `json:"reminder_days_before"`
}

// Update edits a schedule for future dates only (DESIGN.md section 11): occurrences that already happened,
// were confirmed or skipped, or are overdue keep what they had. Changing the amount, account, category or text
// updates the future expectations. Changing the pattern (frequency, interval, end date, time zone, time) drops
// future expectations and regenerates them from today. The start date never changes: it is the anchor.
func (s *Service) Update(ctx context.Context, userID, id string, in UpdateInput) (Schedule, error) {
	if in.Version < 1 {
		return Schedule{}, apierr.Validation(map[string]string{"version": "Send the version you loaded."})
	}
	var out Schedule
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		cur, err := scanSched(tx.QueryRow(ctx, `SELECT `+schedCols+` FROM recurring_schedules s WHERE s.id=$1 AND s.user_id=$2 FOR UPDATE OF s`, id, userID))
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Version != in.Version {
			return ErrVersion
		}
		if cur.Status == "cancelled" {
			return ErrState
		}
		m := Input{AccountID: cur.AccountID, Type: cur.Type, Title: cur.Title, Amount: cur.Amount, CategoryID: cur.CategoryID, PaymentMethod: cur.PaymentMethod,
			PayeeName: cur.PayeeName, Notes: cur.Notes, Frequency: cur.Frequency, Interval: cur.Interval, StartDate: cur.StartDate, EndDate: cur.EndDate,
			Timezone: cur.Timezone, TimeOfDay: cur.TimeOfDay, ReminderDaysBefore: cur.ReminderDaysBefore}
		pattern := false
		if in.AccountID != nil {
			m.AccountID = *in.AccountID
		}
		if in.Title != nil {
			m.Title = *in.Title
		}
		if in.Amount != nil {
			m.Amount = *in.Amount
		}
		if in.CategoryID != nil {
			m.CategoryID = in.CategoryID
		}
		if in.PaymentMethod != nil {
			m.PaymentMethod = *in.PaymentMethod
		}
		if in.PayeeName != nil {
			m.PayeeName = *in.PayeeName
		}
		if in.Notes != nil {
			m.Notes = *in.Notes
		}
		if in.Frequency != nil && *in.Frequency != cur.Frequency {
			m.Frequency, pattern = *in.Frequency, true
		}
		if in.Interval != nil && *in.Interval != cur.Interval {
			m.Interval, pattern = *in.Interval, true
		}
		if in.EndDate != nil {
			if *in.EndDate == "" {
				m.EndDate = nil
			} else {
				m.EndDate = in.EndDate
			}
			pattern = true
		}
		if in.Timezone != nil && *in.Timezone != cur.Timezone {
			m.Timezone, pattern = *in.Timezone, true
		}
		if in.TimeOfDay != nil && *in.TimeOfDay != cur.TimeOfDay {
			m.TimeOfDay, pattern = *in.TimeOfDay, true
		}
		if in.ReminderDaysBefore != nil {
			m.ReminderDaysBefore = in.ReminderDaysBefore
		}
		start, _ := calendar.ParseDate(cur.StartDate)
		r, err := s.resolve(ctx, tx, userID, m, &start)
		if err != nil {
			return err
		}
		now := s.now()
		loc := s.loc(r.in.Timezone)
		if pattern {
			// Drop expectations that have not come due yet; the cursor moves back so they are regenerated.
			if _, err := tx.Exec(ctx, `DELETE FROM recurring_occurrences WHERE schedule_id=$1 AND status='expected' AND due_at > $2`, id, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE recurring_schedules SET generated_through=$2 WHERE id=$1`, id, calendar.Today(now, loc).AddDate(0, 0, -1)); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE recurring_occurrences SET expected_amount=$2, currency=$3 WHERE schedule_id=$1 AND status='expected' AND due_at > $4`,
			id, r.in.Amount, r.currency, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE recurring_schedules SET account_id=$3, title=$4, amount=$5, currency=$6, category_id=$7, payment_method=$8,
			payee_name=$9, notes=$10, frequency=$11, interval=$12, end_date=$13, timezone=$14, time_of_day=$15, reminder_days_before=$16,
			version=version+1, updated_at=now() WHERE id=$1 AND user_id=$2`,
			id, userID, r.in.AccountID, r.in.Title, r.in.Amount, r.currency, r.in.CategoryID, r.in.PaymentMethod, r.in.PayeeName, r.in.Notes,
			r.in.Frequency, r.in.Interval, r.end, r.in.Timezone, r.tod, r.in.ReminderDaysBefore); err != nil {
			return err
		}
		if _, err := s.generate(ctx, tx, id, now); err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "recurrence.updated", map[string]any{"schedule_id": id, "pattern_changed": pattern}); err != nil {
			return err
		}
		out, err = scanSched(tx.QueryRow(ctx, `SELECT `+schedCols+` FROM recurring_schedules s WHERE s.id=$1`, id))
		return err
	})
	return out, err
}

// SetStatus pauses, resumes or cancels a schedule. Pausing and cancelling drop expectations that have not come
// due; overdue ones stay for review. Resuming starts again from today (the paused gap is not backfilled).
func (s *Service) SetStatus(ctx context.Context, userID, id, action string) (Schedule, error) {
	var out Schedule
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var status, tz string
		err := tx.QueryRow(ctx, `SELECT status, timezone FROM recurring_schedules WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, userID).Scan(&status, &tz)
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		now := s.now()
		var next string
		switch action {
		case "pause":
			if status != "active" {
				return ErrState
			}
			next = "paused"
		case "resume":
			if status != "paused" {
				return ErrState
			}
			next = "active"
		case "cancel":
			if status == "cancelled" {
				return ErrState
			}
			next = "cancelled"
		}
		if _, err := tx.Exec(ctx, `UPDATE recurring_schedules SET status=$2, version=version+1, updated_at=now() WHERE id=$1`, id, next); err != nil {
			return err
		}
		switch next {
		case "paused", "cancelled":
			if _, err := tx.Exec(ctx, `DELETE FROM recurring_occurrences WHERE schedule_id=$1 AND status='expected' AND due_at > $2`, id, now); err != nil {
				return err
			}
		case "active":
			if _, err := tx.Exec(ctx, `UPDATE recurring_schedules SET generated_through=$2 WHERE id=$1`,
				id, calendar.Today(now, s.loc(tz)).AddDate(0, 0, -1)); err != nil {
				return err
			}
			if _, err := s.generate(ctx, tx, id, now); err != nil {
				return err
			}
		}
		if err := audit.Log(ctx, tx, userID, "recurrence."+action, map[string]any{"schedule_id": id}); err != nil {
			return err
		}
		out, err = scanSched(tx.QueryRow(ctx, `SELECT `+schedCols+` FROM recurring_schedules s WHERE s.id=$1`, id))
		return err
	})
	return out, err
}

// ListOccurrences returns a schedule's occurrences, soonest first.
func (s *Service) ListOccurrences(ctx context.Context, userID, scheduleID, state string, limit int) ([]Occurrence, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if _, err := s.Get(ctx, userID, scheduleID); err != nil {
		return nil, err
	}
	return s.queryOcc(ctx, `WHERE o.user_id=$1 AND o.schedule_id=$2 AND ($3 = '' OR `+stateExpr+` = $3) ORDER BY o.due_at DESC, o.id LIMIT $4`, userID, scheduleID, state, limit)
}

const stateExpr = `(CASE o.status WHEN 'confirmed' THEN 'confirmed' WHEN 'skipped' THEN 'skipped' ELSE CASE WHEN o.due_at < now() THEN 'overdue' ELSE 'upcoming' END END)`

func (s *Service) queryOcc(ctx context.Context, tail string, args ...any) ([]Occurrence, error) {
	rows, err := s.db.Query(ctx, `SELECT `+occCols+` `+occFrom+` `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Occurrence{}
	for rows.Next() {
		o, err := scanOcc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Occurrences lists expectations across all of the user's schedules.
//
//	upcoming: still to come within `days` (soonest first); overdue: expected but past due (oldest first).
func (s *Service) Occurrences(ctx context.Context, userID, state string, days int) ([]Occurrence, error) {
	if days <= 0 || days > 365 {
		days = 30
	}
	switch state {
	case "overdue":
		return s.queryOcc(ctx, `WHERE o.user_id=$1 AND o.status='expected' AND o.due_at < now() ORDER BY o.due_at LIMIT 200`, userID)
	case "upcoming", "":
		return s.queryOcc(ctx, `WHERE o.user_id=$1 AND o.status='expected' AND o.due_at >= now() AND o.due_at < now() + make_interval(days => $2) ORDER BY o.due_at LIMIT 200`, userID, days)
	}
	return nil, apierr.Validation(map[string]string{"state": "Choose upcoming or overdue."})
}

type ConfirmInput struct {
	AccountID  *string    `json:"account_id"`  // defaults to the schedule's account
	Amount     *int64     `json:"amount"`      // actual amount; defaults to the expected amount
	OccurredAt *time.Time `json:"occurred_at"` // defaults to now
}

// Confirm records what actually happened for an expected occurrence: a real, confirmed transaction created by the
// transactions module. This is the only way a recurring schedule changes a balance, and it is always the user's
// action. The same occurrence can never produce a second transaction (status check and source-event index).
func (s *Service) Confirm(ctx context.Context, userID, key, scheduleID, occID string, in ConfirmInput) (Occurrence, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Occurrence{}, err
	}
	var out Occurrence
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "recurrence.confirm", idempotency.Hash("recurrence.confirm", []any{scheduleID, occID, in}))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = scanOcc(tx.QueryRow(ctx, `SELECT `+occCols+` `+occFrom+` WHERE o.id=$1 AND o.user_id=$2`, replay.ResourceID, userID))
			return err
		}
		var status, typ, title, payee, notes, pm, tz, acct, cur string
		var catID *string
		var expected int64
		err = tx.QueryRow(ctx, `SELECT o.status, s.type, s.title, s.payee_name, s.notes, s.payment_method, s.timezone, s.account_id::text, s.category_id::text, o.expected_amount, o.currency
			FROM recurring_occurrences o JOIN recurring_schedules s ON s.id = o.schedule_id
			WHERE o.id=$1 AND o.schedule_id=$2 AND o.user_id=$3 FOR UPDATE OF o`, occID, scheduleID, userID).
			Scan(&status, &typ, &title, &payee, &notes, &pm, &tz, &acct, &catID, &expected, &cur)
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != "expected" {
			return ErrClosed
		}
		n := transactions.NewTx{AccountID: acct, Type: typ, Amount: expected, Title: title, CategoryID: catID, PaymentMethod: pm, PayeeName: payee,
			Notes: notes, Status: "confirmed", OccurredAt: s.now().UTC(), Timezone: tz, Source: "recurring", ExternalRef: "recurrence:" + occID}
		if in.AccountID != nil {
			n.AccountID = ids.Normalize(*in.AccountID)
		}
		if in.Amount != nil {
			n.Amount = *in.Amount
		}
		if in.OccurredAt != nil {
			n.OccurredAt = in.OccurredAt.UTC()
		}
		txID, err := s.txs.CreateInTx(ctx, tx, userID, n)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE recurring_occurrences SET status='confirmed', transaction_id=$2, resolved_at=now() WHERE id=$1`, occID, txID); err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "recurrence.occurrence_confirmed", map[string]any{"occurrence_id": occID, "transaction_id": txID}); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "occurrence", occID); err != nil {
			return err
		}
		out, err = scanOcc(tx.QueryRow(ctx, `SELECT `+occCols+` `+occFrom+` WHERE o.id=$1`, occID))
		return err
	})
	return out, err
}

// Skip marks an expected occurrence as not happening. It records nothing in the ledger.
func (s *Service) Skip(ctx context.Context, userID, scheduleID, occID string) (Occurrence, error) {
	var out Occurrence
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM recurring_occurrences WHERE id=$1 AND schedule_id=$2 AND user_id=$3 FOR UPDATE`, occID, scheduleID, userID).Scan(&status)
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != "expected" {
			return ErrClosed
		}
		if _, err := tx.Exec(ctx, `UPDATE recurring_occurrences SET status='skipped', resolved_at=now() WHERE id=$1`, occID); err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "recurrence.occurrence_skipped", map[string]any{"occurrence_id": occID}); err != nil {
			return err
		}
		out, err = scanOcc(tx.QueryRow(ctx, `SELECT `+occCols+` `+occFrom+` WHERE o.id=$1`, occID))
		return err
	})
	return out, err
}

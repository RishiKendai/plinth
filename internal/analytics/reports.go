package analytics

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"plinth/server/internal/audit"
	"plinth/server/internal/idempotency"
	"plinth/server/internal/imports"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
)

const (
	maxReportRows = 200_000
	maxAttempts   = 3
	maxWorking    = 3
	staleRunning  = 10 * time.Minute
)

type Report struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	AccountID  *string    `json:"account_id"`
	From       string     `json:"from"`
	To         string     `json:"to"`
	Status     string     `json:"status"` // queued | running | ready | failed
	RowCount   int        `json:"row_count"`
	Error      string     `json:"error"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

type ReportInput struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	AccountID string `json:"account_id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Year      int    `json:"year"`
}

const reportCols = `id::text, kind, account_id::text, to_char(period_from,'YYYY-MM-DD'), to_char(period_to,'YYYY-MM-DD'), status, row_count, error, created_at, finished_at`

func scanReport(r pgx.Row) (Report, error) {
	var x Report
	err := r.Scan(&x.ID, &x.Kind, &x.AccountID, &x.From, &x.To, &x.Status, &x.RowCount, &x.Error, &x.CreatedAt, &x.FinishedAt)
	x.CreatedAt = x.CreatedAt.UTC()
	return x, err
}

// CreateReport queues a report. Generation happens in the background (worker, or Process called right after); the caller
// polls Get. A retry with the same Idempotency-Key returns the same report.
func (s *Service) CreateReport(ctx context.Context, userID, key string, in ReportInput) (Report, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Report{}, err
	}
	f := map[string]string{}
	if in.Kind != "statement" && in.Kind != "year_in_review" {
		f["kind"] = "Choose a statement or a year in review."
	}
	from, to := in.From, in.To
	if in.Kind == "year_in_review" {
		y := in.Year
		if y == 0 {
			y = s.now().Year()
		}
		if y < 2000 || y > s.now().Year() {
			f["year"] = "Choose a year from 2000 to this year."
		}
		from, to = fmt.Sprintf("%04d-01-01", y), fmt.Sprintf("%04d-12-31", y)
	} else if in.Kind == "statement" {
		if in.AccountID == "" {
			f["account_id"] = "Choose an account."
		}
		if from == "" || to == "" {
			f["from"] = "Choose a start and end date."
		}
	}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		f["id"] = "Identifier must be a UUID."
	}
	if id == "" {
		id = ids.New()
	}
	if len(f) > 0 {
		return Report{}, apierr.Validation(f)
	}
	sc, err := s.Resolve(ctx, userID, from, to, "", in.AccountID)
	if err != nil {
		return Report{}, err
	}
	var out Report
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "report.create", idempotency.Hash("report.create", in))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = scanReport(tx.QueryRow(ctx, `SELECT `+reportCols+` FROM reports WHERE id=$1 AND user_id=$2`, replay.ResourceID, userID))
			return err
		}
		// Reports are built from the whole ledger, so a few at a time is plenty. This stops a retry loop from piling up work.
		var working int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reports WHERE user_id=$1 AND status IN ('queued','running')`, userID).Scan(&working); err != nil {
			return err
		}
		if working >= maxWorking {
			return apierr.New(429, "too_many_reports", "Some reports are still being prepared. Wait for them to finish, then try again.")
		}
		var acct any
		if sc.AccountID != nil {
			acct = *sc.AccountID
		}
		if in.Kind == "year_in_review" {
			acct = nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO reports (id, user_id, kind, account_id, period_from, period_to) VALUES ($1,$2,$3,$4::uuid,$5::date,$6::date)`, id, userID, in.Kind, acct, sc.FromDate, sc.ToDate); err != nil {
			if dbx.ConstraintName(err) == "reports_pkey" {
				return apierr.New(409, "id_taken", "That identifier is already used.")
			}
			return err
		}
		if err := audit.Log(ctx, tx, userID, "report.created", map[string]any{"report_id": id, "kind": in.Kind}); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "report", id); err != nil {
			return err
		}
		out, err = scanReport(tx.QueryRow(ctx, `SELECT `+reportCols+` FROM reports WHERE id=$1`, id))
		return err
	})
	return out, err
}

func (s *Service) GetReport(ctx context.Context, userID, id string) (Report, error) {
	r, err := scanReport(s.db.QueryRow(ctx, `SELECT `+reportCols+` FROM reports WHERE id=$1 AND user_id=$2`, id, userID))
	if dbx.IsNoRows(err) {
		return r, apierr.ErrNotFound
	}
	return r, err
}

func (s *Service) ListReports(ctx context.Context, userID string) ([]Report, error) {
	rows, err := s.db.Query(ctx, `SELECT `+reportCols+` FROM reports WHERE user_id=$1 ORDER BY created_at DESC, id LIMIT 50`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Report{}
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Content returns a ready report's file. Reports are sensitive: handlers send it with no-store.
func (s *Service) Content(ctx context.Context, userID, id string) (string, string, error) {
	var kind, status string
	var content *string
	var from, to string
	err := s.db.QueryRow(ctx, `SELECT kind, status, content, to_char(period_from,'YYYY-MM-DD'), to_char(period_to,'YYYY-MM-DD') FROM reports WHERE id=$1 AND user_id=$2`, id, userID).Scan(&kind, &status, &content, &from, &to)
	if dbx.IsNoRows(err) {
		return "", "", apierr.ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	if status != "ready" || content == nil {
		return "", "", apierr.New(409, "report_not_ready", "This report is not ready yet.")
	}
	return *content, fmt.Sprintf("plinth-%s-%s-to-%s.csv", kind, from, to), nil
}

// Process generates one report. It is safe to call twice and safe to retry: a report already ready or failed is left alone,
// and one that crashed mid-run is picked up again after staleRunning.
func (s *Service) Process(ctx context.Context, id string) error {
	var userID, kind, from, to string
	var acct *string
	var attempts int
	err := s.db.QueryRow(ctx, `UPDATE reports SET status='running', attempts=attempts+1, started_at=now()
		WHERE id=$1 AND (status='queued' OR (status='running' AND started_at < now() - $2::interval)) AND attempts < $3
		RETURNING user_id::text, kind, account_id::text, to_char(period_from,'YYYY-MM-DD'), to_char(period_to,'YYYY-MM-DD'), attempts`,
		id, staleRunning.String(), maxAttempts).Scan(&userID, &kind, &acct, &from, &to, &attempts)
	if dbx.IsNoRows(err) {
		return nil // somebody else has it, or it is finished
	}
	if err != nil {
		return err
	}
	acctID := ""
	if acct != nil {
		acctID = *acct
	}
	content, rows, genErr := s.generate(ctx, userID, kind, acctID, from, to)
	if genErr != nil {
		status, msg := "queued", ""
		if attempts >= maxAttempts {
			status, msg = "failed", "The report could not be created. Try again later."
		}
		if _, err := s.db.Exec(ctx, `UPDATE reports SET status=$2, error=$3 WHERE id=$1`, id, status, msg); err != nil {
			return err
		}
		return genErr
	}
	_, err = s.db.Exec(ctx, `UPDATE reports SET status='ready', content=$2, row_count=$3, error='', finished_at=now() WHERE id=$1`, id, content, rows)
	return err
}

// ProcessQueued is the worker's sweep. It is bounded and each report is independent.
func (s *Service) ProcessQueued(ctx context.Context, limit int, log *slog.Logger) (int, error) {
	rows, err := s.db.Query(ctx, `SELECT id::text FROM reports WHERE (status='queued' OR (status='running' AND started_at < now() - $2::interval)) AND attempts < $3 ORDER BY created_at LIMIT $1`,
		limit, staleRunning.String(), maxAttempts)
	if err != nil {
		return 0, err
	}
	var queue []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		queue = append(queue, id)
	}
	rows.Close()
	done := 0
	for _, id := range queue {
		if err := s.Process(ctx, id); err != nil {
			log.Warn("report failed", "report_id", id, "err", err.Error())
			continue
		}
		done++
	}
	return done, nil
}

func (s *Service) generate(ctx context.Context, userID, kind, accountID, from, to string) (string, int, error) {
	if kind == "statement" {
		return s.statement(ctx, userID, accountID, from, to)
	}
	return s.yearInReview(ctx, userID, from, to)
}

// statement lists every ledger movement of one account in the range with the balance after each one. It is built from the
// ledger (opening balance plus entries), so it reconciles with the account balance by construction.
func (s *Service) statement(ctx context.Context, userID, accountID, from, to string) (string, int, error) {
	sc, err := s.Resolve(ctx, userID, from, to, "", accountID)
	if err != nil {
		return "", 0, err
	}
	var cur string
	var opening int64
	if err := s.db.QueryRow(ctx, `SELECT currency, opening_balance FROM accounts WHERE id=$1 AND user_id=$2`, *sc.AccountID, userID).Scan(&cur, &opening); err != nil {
		return "", 0, apierr.ErrNotFound
	}
	var before int64
	if err := s.db.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0)::bigint FROM ledger_entries WHERE account_id=$1 AND effective_at < $2`, *sc.AccountID, sc.From).Scan(&before); err != nil {
		return "", 0, err
	}
	bal := opening + before
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"Date", "Description", "Category", "Status", "Money out", "Money in", "Balance"})
	_ = w.Write([]string{sc.FromDate, "Opening balance", "", "", "", "", plain(bal, cur)})
	rows, err := s.db.Query(ctx, `SELECT to_char(e.effective_at AT TIME ZONE $4::text,'YYYY-MM-DD'), t.title, COALESCE(c.name,''), t.status, e.amount
		FROM ledger_entries e JOIN transactions t ON t.id = e.transaction_id LEFT JOIN categories c ON c.id = t.category_id
		WHERE e.account_id=$1 AND e.user_id=$2 AND e.effective_at >= $3 AND e.effective_at < $5 ORDER BY e.effective_at, e.id LIMIT $6`,
		*sc.AccountID, userID, sc.From, sc.TZ, sc.To, maxReportRows+1)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var d, title, cat, status string
		var amt int64
		if err := rows.Scan(&d, &title, &cat, &status, &amt); err != nil {
			return "", 0, err
		}
		n++
		if n > maxReportRows {
			return "", 0, fmt.Errorf("report too large")
		}
		bal += amt
		out, in := "", ""
		if amt < 0 {
			out = plain(-amt, cur)
		} else {
			in = plain(amt, cur)
		}
		_ = w.Write([]string{d, imports.SafeCell(title), imports.SafeCell(cat), status, out, in, plain(bal, cur)})
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	_ = w.Write([]string{sc.ToDate, "Closing balance", "", "", "", "", plain(bal, cur)})
	w.Flush()
	return buf.String(), n, w.Error()
}

// yearInReview is a month table and a category table for one calendar year, from the same facts as the charts.
func (s *Service) yearInReview(ctx context.Context, userID, from, to string) (string, int, error) {
	sc, err := s.Resolve(ctx, userID, from, to, "", "")
	if err != nil {
		return "", 0, err
	}
	months, err := s.Trend(ctx, sc, "month")
	if err != nil {
		return "", 0, err
	}
	cats, total, err := s.Breakdown(ctx, sc, "category", "expense", 100)
	if err != nil {
		return "", 0, err
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"Year in review", sc.FromDate[:4], sc.Currency})
	_ = w.Write([]string{"Month", "Money in", "Money out", "Net"})
	var in, out int64
	for _, m := range months {
		_ = w.Write([]string{m.Date[:7], plain(m.Income, sc.Currency), plain(m.Expense, sc.Currency), plain(m.Net, sc.Currency)})
		in += m.Income
		out += m.Expense
	}
	_ = w.Write([]string{"Total", plain(in, sc.Currency), plain(out, sc.Currency), plain(in-out, sc.Currency)})
	_ = w.Write(nil)
	_ = w.Write([]string{"Category", "Spent", "Share"})
	for _, c := range cats {
		_ = w.Write([]string{imports.SafeCell(c.Label), plain(c.Amount, sc.Currency), fmt.Sprintf("%d.%02d%%", c.Percent/100, c.Percent%100)})
	}
	_ = w.Write([]string{"Total", plain(total, sc.Currency), ""})
	w.Flush()
	return buf.String(), len(months) + len(cats), w.Error()
}

func contextBackground() context.Context { return context.Background() }

// DeleteReport removes a report and its stored file. Reports are derived copies of the ledger (the ledger itself is never
// touched), so the owner may delete them whenever they like; no retention period is imposed.
func (s *Service) DeleteReport(ctx context.Context, userID, id string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM reports WHERE id=$1 AND user_id=$2`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apierr.ErrNotFound
	}
	return nil
}

// Package upi tracks UPI payment attempts (CLAUDE.md 3.6). Plinth is not a payment processor and never sees a UPI
// PIN: the user pays in their own UPI app, and Plinth only records what the USER tells it afterwards.
//
// An attempt moves through four distinct steps and none implies the next:
//
//	started      the user scanned a QR and reviewed the payee
//	handed_off   Plinth opened the UPI app (launching an app proves nothing about payment)
//	paid | failed | uncertain   what the user says happened
//	recorded     a transaction exists (paid: confirmed; uncertain: pending, with a reminder)
//
// "Bank verified" is set only when a matching bank SMS arrives (see internal/bankmsg).
package upi

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/accounts"
	"plinth/server/internal/audit"
	"plinth/server/internal/idempotency"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
	"plinth/server/internal/reminders"
	"plinth/server/internal/transactions"
)

var vpaRe = regexp.MustCompile(`^[A-Za-z0-9._-]{2,256}@[A-Za-z]{2,64}$`)

var ErrState = apierr.New(http.StatusConflict, "invalid_state", "That payment is not in a state that allows this.")

type Payment struct {
	ID             string     `json:"id"`
	AccountID      string     `json:"account_id"`
	PayeeVPA       string     `json:"payee_vpa"`
	PayeeName      string     `json:"payee_name"`
	Amount         int64      `json:"amount"`
	Currency       string     `json:"currency"`
	Note           string     `json:"note"`
	TxnRef         string     `json:"txn_ref"`
	Status         string     `json:"status"` // started | handed_off | paid | failed | uncertain
	Step           string     `json:"step"`   // started | in_upi_app | confirmed_by_you | not_completed | not_sure | recorded
	TransactionID  *string    `json:"transaction_id"`
	BankVerified   bool       `json:"bank_verified"`
	BankVerifiedAt *time.Time `json:"bank_verified_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

type Service struct {
	db        *pgxpool.Pool
	txs       *transactions.Service
	reminders *reminders.Service
	now       func() time.Time
}

func NewService(db *pgxpool.Pool, txs *transactions.Service, rem *reminders.Service) *Service {
	return &Service{db: db, txs: txs, reminders: rem, now: time.Now}
}

const cols = `id::text, account_id::text, payee_vpa, payee_name, amount, currency, note, txn_ref, status, transaction_id::text, bank_verified_at, created_at`

func scan(row interface{ Scan(...any) error }) (Payment, error) {
	var p Payment
	err := row.Scan(&p.ID, &p.AccountID, &p.PayeeVPA, &p.PayeeName, &p.Amount, &p.Currency, &p.Note, &p.TxnRef, &p.Status, &p.TransactionID, &p.BankVerifiedAt, &p.CreatedAt)
	p.CreatedAt = p.CreatedAt.UTC()
	p.BankVerified = p.BankVerifiedAt != nil
	switch {
	case p.TransactionID != nil:
		p.Step = "recorded"
	case p.Status == "handed_off":
		p.Step = "in_upi_app"
	case p.Status == "failed":
		p.Step = "not_completed"
	default:
		p.Step = p.Status
	}
	return p, err
}

type StartInput struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	PayeeVPA  string `json:"payee_vpa"`
	PayeeName string `json:"payee_name"`
	Amount    int64  `json:"amount"`
	Note      string `json:"note"`
	TxnRef    string `json:"txn_ref"`
}

// Start records that the user is about to pay. It creates no transaction.
func (s *Service) Start(ctx context.Context, userID string, in StartInput) (Payment, error) {
	f := map[string]string{}
	if !vpaRe.MatchString(in.PayeeVPA) {
		f["payee_vpa"] = "That does not look like a UPI ID, such as name@bank."
	}
	if in.Amount <= 0 || in.Amount > 1_000_000_000_000_000 {
		f["amount"] = "Enter the amount you are paying."
	}
	in.PayeeName, in.Note, in.TxnRef = strings.TrimSpace(in.PayeeName), strings.TrimSpace(in.Note), strings.TrimSpace(in.TxnRef)
	if utf8.RuneCountInString(in.PayeeName) > 120 {
		f["payee_name"] = "Use 120 characters or fewer."
	}
	if utf8.RuneCountInString(in.Note) > 120 {
		f["note"] = "Use 120 characters or fewer."
	}
	if utf8.RuneCountInString(in.TxnRef) > 60 {
		f["txn_ref"] = "Reference is too long."
	}
	aid := ids.Normalize(in.AccountID)
	if !ids.Valid(aid) {
		f["account_id"] = "Choose the account you are paying from."
	}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		f["id"] = "Identifier must be a UUID."
	}
	if len(f) > 0 {
		return Payment{}, apierr.Validation(f)
	}
	if id == "" {
		id = ids.New()
	}
	acct, err := accounts.Fetch(ctx, s.db, userID, aid)
	if err != nil || acct.ArchivedAt != nil {
		return Payment{}, apierr.Validation(map[string]string{"account_id": "Choose one of your active accounts."})
	}
	if p, err := s.Get(ctx, userID, id); err == nil {
		return p, nil // the same start sent again
	}
	p, err := scan(s.db.QueryRow(ctx, `INSERT INTO upi_payments (id, user_id, account_id, payee_vpa, payee_name, amount, currency, note, txn_ref)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+cols, id, userID, aid, strings.ToLower(in.PayeeVPA), in.PayeeName, in.Amount, acct.Currency, in.Note, in.TxnRef))
	if err != nil {
		if dbx.ConstraintName(err) == "upi_payments_pkey" {
			return Payment{}, transactions.ErrIDTaken
		}
		return Payment{}, err
	}
	return p, audit.Log(ctx, s.db, userID, "upi.started", map[string]any{"payment_id": id})
}

func (s *Service) Get(ctx context.Context, userID, id string) (Payment, error) {
	p, err := scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM upi_payments WHERE id=$1 AND user_id=$2`, id, userID))
	if dbx.IsNoRows(err) {
		return p, apierr.ErrNotFound
	}
	return p, err
}

func (s *Service) List(ctx context.Context, userID string) ([]Payment, error) {
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM upi_payments WHERE user_id=$1 ORDER BY created_at DESC LIMIT 100`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Payment{}
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Delete removes a payment attempt from the list. When voidTransaction is set and a transaction was recorded from it, that
// transaction is voided (cancelled, or reversed if it was confirmed) in the same database transaction, so the ledger stays explainable.
// Without it the recorded transaction is kept and only the attempt record goes.
func (s *Service) Delete(ctx context.Context, userID, id string, voidTransaction bool) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		p, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM upi_payments WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, userID))
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if voidTransaction && p.TransactionID != nil {
			if err := s.txs.VoidInTx(ctx, tx, userID, *p.TransactionID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM upi_payments WHERE id=$1 AND user_id=$2`, id, userID); err != nil {
			return err
		}
		return audit.Log(ctx, tx, userID, "upi.deleted", map[string]any{"payment_id": id, "transaction_voided": voidTransaction && p.TransactionID != nil})
	})
}

// Handoff notes that the UPI app was opened. It changes no money and records no transaction.
func (s *Service) Handoff(ctx context.Context, userID, id string) (Payment, error) {
	p, err := scan(s.db.QueryRow(ctx, `UPDATE upi_payments SET status='handed_off', updated_at=now() WHERE id=$1 AND user_id=$2 AND status IN ('started','handed_off') RETURNING `+cols, id, userID))
	if dbx.IsNoRows(err) {
		if _, gerr := s.Get(ctx, userID, id); gerr != nil {
			return Payment{}, gerr
		}
		return Payment{}, ErrState
	}
	return p, err
}

// Outcome records what the user says happened. paid creates a confirmed expense; not_sure creates a PENDING expense
// and a reminder to check; failed records nothing. An uncertain payment can later be resolved to paid or failed.
func (s *Service) Outcome(ctx context.Context, userID, key, id, outcome string) (Payment, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Payment{}, err
	}
	if outcome != "paid" && outcome != "failed" && outcome != "not_sure" {
		return Payment{}, apierr.Validation(map[string]string{"outcome": "Choose paid, failed or not_sure."})
	}
	var out Payment
	var reminderFor *Payment
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "upi.outcome", idempotency.Hash("upi.outcome", []string{id, outcome}))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM upi_payments WHERE id=$1 AND user_id=$2`, replay.ResourceID, userID))
			return err
		}
		p, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM upi_payments WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, userID))
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case p.Status == "paid" || p.Status == "failed":
			return ErrState
		case p.Status == "uncertain" && outcome == "not_sure":
			return ErrState
		}
		newStatus := map[string]string{"paid": "paid", "failed": "failed", "not_sure": "uncertain"}[outcome]
		txID := p.TransactionID
		switch {
		case p.Status == "uncertain" && txID != nil:
			// Resolving a pending payment: confirm or fail the same transaction (idempotency keys derive from ours).
			to := "confirmed"
			if outcome == "failed" {
				to = "failed"
			}
			if _, err := s.txs.ChangeStatusInTx(ctx, tx, userID, *txID, to); err != nil {
				return err
			}
		case outcome == "paid" || outcome == "not_sure":
			status := "confirmed"
			if outcome == "not_sure" {
				status = "pending"
			}
			title := "Paid " + p.PayeeName
			if p.PayeeName == "" {
				title = "Paid " + p.PayeeVPA
			}
			ref := p.TxnRef
			if ref == "" {
				ref = p.ID
			}
			id2, err := s.txs.CreateInTx(ctx, tx, userID, transactions.NewTx{AccountID: p.AccountID, Type: "expense", Amount: p.Amount, Title: title,
				PaymentMethod: "upi", PayeeName: p.PayeeName, Status: status, OccurredAt: s.now().UTC(), Timezone: "Asia/Kolkata", Source: "upi", ExternalRef: "upi:" + ref, Notes: p.Note})
			if err != nil {
				return err
			}
			txID = &id2
		}
		out, err = scan(tx.QueryRow(ctx, `UPDATE upi_payments SET status=$3, transaction_id=$4, updated_at=now() WHERE id=$1 AND user_id=$2 RETURNING `+cols, id, userID, newStatus, txID))
		if err != nil {
			return err
		}
		if outcome == "not_sure" {
			reminderFor = &out
		}
		if err := audit.Log(ctx, tx, userID, "upi.outcome", map[string]any{"payment_id": id, "outcome": outcome}); err != nil {
			return err
		}
		return idempotency.Complete(ctx, tx, userID, key, "upi_payment", id)
	})
	if err != nil {
		return out, err
	}
	if reminderFor != nil && s.reminders != nil {
		// "Uncertain = pending, with a reminder": a nudge to check the bank. Best effort; the pending transaction is the record.
		_, _ = s.reminders.Create(ctx, userID, "", reminders.Input{Title: "Check your UPI payment to " + firstNonEmpty(reminderFor.PayeeName, reminderFor.PayeeVPA),
			DueDate: s.now().AddDate(0, 0, 1).Format("2006-01-02"), Notes: "You were not sure this payment went through. Check your bank, then mark it paid or failed."})
	}
	return out, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// VerifyBySMS marks a payment bank-verified when a bank debit message matches it unambiguously: the same amount, a
// debit, received within a day of the attempt, and exactly one candidate. An uncertain (pending) payment is also
// confirmed, because the bank itself says the money left. It returns the matched payment's transaction, if any.
func (s *Service) VerifyBySMS(ctx context.Context, tx pgx.Tx, userID string, amount int64, receivedAt time.Time) (paymentID string, transactionID string, ok bool, err error) {
	rows, err := tx.Query(ctx, `SELECT id::text, COALESCE(transaction_id::text,''), status FROM upi_payments
		WHERE user_id=$1 AND status IN ('paid','uncertain') AND bank_verified_at IS NULL AND amount=$2
		  AND created_at BETWEEN $3 AND $4
		ORDER BY created_at`, userID, amount, receivedAt.Add(-24*time.Hour), receivedAt.Add(24*time.Hour))
	if err != nil {
		return "", "", false, err
	}
	type c struct{ id, tx, status string }
	var cs []c
	for rows.Next() {
		var x c
		if err := rows.Scan(&x.id, &x.tx, &x.status); err != nil {
			rows.Close()
			return "", "", false, err
		}
		cs = append(cs, x)
	}
	rows.Close()
	if len(cs) != 1 { // none, or ambiguous: never guess
		return "", "", false, nil
	}
	m := cs[0]
	if _, err := tx.Exec(ctx, `UPDATE upi_payments SET bank_verified_at=now(), updated_at=now() WHERE id=$1`, m.id); err != nil {
		return "", "", false, err
	}
	if m.status == "uncertain" && m.tx != "" {
		if _, err := s.txs.ChangeStatusInTx(ctx, tx, userID, m.tx, "confirmed"); err != nil {
			return "", "", false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE upi_payments SET status='paid' WHERE id=$1`, m.id); err != nil {
			return "", "", false, err
		}
	}
	return m.id, m.tx, true, nil
}

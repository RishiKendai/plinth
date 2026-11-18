// Package bankmsg turns bank and UPI messages into reviewable suggestions (CLAUDE.md 3.7).
//
// The message is read and parsed ON THE PHONE; only the parsed fields and a hash of the text reach the server, which
// never receives or stores the message text. Plinth does not read the SMS inbox: the user shares or pastes a message.
//
// Rules: an event is never added to an account automatically. An account is only SUGGESTED when its last four digits
// match the message and exactly one account does; otherwise the user chooses. A message already processed, or one that
// matches a transaction the user already has, is linked and shown, never silently dropped or double-counted.
package bankmsg

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/accounts"
	"plinth/server/internal/audit"
	"plinth/server/internal/idempotency"
	"plinth/server/internal/imports"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
	"plinth/server/internal/transactions"
	"plinth/server/internal/upi"
)

var ErrState = apierr.New(http.StatusConflict, "invalid_state", "That message has already been handled.")

type Event struct {
	ID                 string    `json:"id"`
	Sender             string    `json:"sender"`
	ReceivedAt         time.Time `json:"received_at"`
	Amount             int64     `json:"amount"`
	Direction          string    `json:"direction"`
	Currency           string    `json:"currency"`
	AccountHint        string    `json:"account_hint"`
	Merchant           string    `json:"merchant"`
	Reference          string    `json:"reference"`
	Confidence         int       `json:"confidence"`
	SuggestedAccountID *string   `json:"suggested_account_id"`
	AccountID          *string   `json:"account_id"`
	Status             string    `json:"status"` // review | added | linked | duplicate | dismissed
	TransactionID      *string   `json:"transaction_id"`
	DuplicateOf        *string   `json:"duplicate_of"`
	UPIPaymentID       *string   `json:"upi_payment_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

type Service struct {
	db  *pgxpool.Pool
	txs *transactions.Service
	upi *upi.Service
	now func() time.Time
}

func NewService(db *pgxpool.Pool, txs *transactions.Service, u *upi.Service) *Service {
	return &Service{db: db, txs: txs, upi: u, now: time.Now}
}

const cols = `id::text, sender, received_at, amount, direction, currency, account_hint, merchant, reference, confidence,
	suggested_account_id::text, account_id::text, status, transaction_id::text, duplicate_of::text, created_at`

func scan(row interface{ Scan(...any) error }) (Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.Sender, &e.ReceivedAt, &e.Amount, &e.Direction, &e.Currency, &e.AccountHint, &e.Merchant, &e.Reference, &e.Confidence,
		&e.SuggestedAccountID, &e.AccountID, &e.Status, &e.TransactionID, &e.DuplicateOf, &e.CreatedAt)
	e.ReceivedAt, e.CreatedAt = e.ReceivedAt.UTC(), e.CreatedAt.UTC()
	return e, err
}

type IngestInput struct {
	Fingerprint string    `json:"fingerprint"` // SHA-256 hex of the normalised message text, computed on the phone
	Sender      string    `json:"sender"`
	ReceivedAt  time.Time `json:"received_at"`
	Amount      int64     `json:"amount"`
	Direction   string    `json:"direction"`
	Currency    string    `json:"currency"`
	AccountHint string    `json:"account_hint"` // last four characters of the account or card number in the message
	Merchant    string    `json:"merchant"`
	Reference   string    `json:"reference"`
	Confidence  int       `json:"confidence"`
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Ingest stores a parsed message as a review item, applying the layered duplicate checks. It is idempotent: the same
// fingerprint always returns the same event.
func (s *Service) Ingest(ctx context.Context, userID string, in IngestInput) (Event, error) {
	f := map[string]string{}
	in.Fingerprint = strings.ToLower(in.Fingerprint)
	if !isHex64(in.Fingerprint) {
		f["fingerprint"] = "Send the SHA-256 hash of the message as 64 hex characters."
	}
	if in.Direction != "debit" && in.Direction != "credit" {
		f["direction"] = "Choose debit or credit."
	}
	if in.Amount <= 0 || in.Amount > 1_000_000_000_000_000 {
		f["amount"] = "The message must carry an amount greater than zero."
	}
	if in.ReceivedAt.IsZero() || in.ReceivedAt.After(s.now().Add(24*time.Hour)) {
		f["received_at"] = "The message time is not valid."
	}
	in.AccountHint = strings.TrimSpace(in.AccountHint)
	if len(in.AccountHint) > 4 || (in.AccountHint != "" && !alnum(in.AccountHint)) {
		f["account_hint"] = "Send only the last four characters of the account number."
	}
	in.Merchant, in.Reference, in.Sender = strings.TrimSpace(in.Merchant), strings.TrimSpace(in.Reference), strings.TrimSpace(in.Sender)
	if utf8.RuneCountInString(in.Merchant) > 120 {
		in.Merchant = string([]rune(in.Merchant)[:120])
	}
	if len(in.Reference) > 60 {
		f["reference"] = "Reference is too long."
	}
	if len(in.Sender) > 40 {
		in.Sender = in.Sender[:40]
	}
	if in.Currency == "" {
		in.Currency = "INR"
	}
	if in.Confidence < 0 || in.Confidence > 100 {
		f["confidence"] = "Confidence is a number from 0 to 100."
	}
	if len(f) > 0 {
		return Event{}, apierr.Validation(f)
	}
	var out Event
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		id := ids.New()
		tag, err := tx.Exec(ctx, `INSERT INTO sms_events (id, user_id, fingerprint, sender, received_at, amount, direction, currency, account_hint, merchant, reference, confidence)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT (user_id, fingerprint) DO NOTHING`,
			id, userID, in.Fingerprint, in.Sender, in.ReceivedAt.UTC(), in.Amount, in.Direction, in.Currency, in.AccountHint, in.Merchant, in.Reference, in.Confidence)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 { // the same message again: return what we already have
			out, err = scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM sms_events WHERE user_id=$1 AND fingerprint=$2`, userID, in.Fingerprint))
			return err
		}
		// Account suggestion: only on evidence, and only if it is unique.
		var suggested *string
		if in.AccountHint != "" {
			rows, err := tx.Query(ctx, `SELECT id::text FROM accounts WHERE user_id=$1 AND archived_at IS NULL AND currency=$2 AND lower(masked_identifier)=lower($3)`, userID, in.Currency, in.AccountHint)
			if err != nil {
				return err
			}
			var matches []string
			for rows.Next() {
				var a string
				if err := rows.Scan(&a); err != nil {
					rows.Close()
					return err
				}
				matches = append(matches, a)
			}
			rows.Close()
			if len(matches) == 1 {
				suggested = &matches[0]
			}
		}
		status, txID, dupOf := "review", (*string)(nil), (*string)(nil)
		// Layer 1: a UPI payment the user made through Plinth. The bank message verifies it.
		if in.Direction == "debit" && s.upi != nil {
			if _, t, ok, err := s.upi.VerifyBySMS(ctx, tx, userID, in.Amount, in.ReceivedAt); err != nil {
				return err
			} else if ok {
				status = "linked"
				if t != "" {
					txID = &t
				}
			}
		}
		// Layer 2: the bank reference already backs a transaction (this app, a CSV import, another message).
		if status == "review" && len(in.Reference) >= 6 {
			var t string
			err := tx.QueryRow(ctx, `SELECT id::text FROM transactions WHERE user_id=$1 AND status IN ('pending','confirmed') AND external_ref <> '' AND (external_ref LIKE '%:'||$2 OR external_ref = $2) LIMIT 1`, userID, in.Reference).Scan(&t)
			if err == nil {
				status, dupOf = "duplicate", &t
			} else if !dbx.IsNoRows(err) {
				return err
			}
		}
		// Layer 3: same amount and direction within a day on a plausible account, with a similar merchant or an exact account match.
		if status == "review" {
			typ := "expense"
			if in.Direction == "credit" {
				typ = "income"
			}
			rows, err := tx.Query(ctx, `SELECT id::text, account_id::text, title, payee_name FROM transactions WHERE user_id=$1 AND status IN ('pending','confirmed') AND type=$2 AND amount=$3 AND currency=$4
				AND occurred_at BETWEEN $5 AND $6 ORDER BY occurred_at LIMIT 20`, userID, typ, in.Amount, in.Currency, in.ReceivedAt.Add(-36*time.Hour), in.ReceivedAt.Add(36*time.Hour))
			if err != nil {
				return err
			}
			nm := imports.Normalize(in.Merchant)
			for rows.Next() {
				var tid, acct, title, payee string
				if err := rows.Scan(&tid, &acct, &title, &payee); err != nil {
					rows.Close()
					return err
				}
				similar := nm != "" && (strings.Contains(imports.Normalize(title), nm) || strings.Contains(nm, imports.Normalize(title)) || strings.Contains(imports.Normalize(payee), nm))
				sameAcct := suggested != nil && *suggested == acct
				if similar || sameAcct {
					status, dupOf = "duplicate", &tid
					break
				}
			}
			rows.Close()
		}
		out, err = scan(tx.QueryRow(ctx, `UPDATE sms_events SET suggested_account_id=$2, status=$3, transaction_id=$4, duplicate_of=$5 WHERE id=$1 RETURNING `+cols, id, suggested, status, txID, dupOf))
		if err != nil {
			return err
		}
		return audit.Log(ctx, tx, userID, "sms.ingested", map[string]any{"event_id": id, "status": status})
	})
	return out, err
}

func alnum(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

func (s *Service) List(ctx context.Context, userID, status string) ([]Event, error) {
	switch status {
	case "", "review", "added", "linked", "duplicate", "dismissed":
	default:
		return nil, apierr.Validation(map[string]string{"status": "Choose review, added, linked, duplicate or dismissed."})
	}
	if status == "" {
		status = "review"
	}
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM sms_events WHERE user_id=$1 AND status=$2 ORDER BY received_at DESC, id LIMIT 200`, userID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type AddInput struct {
	AccountID  string  `json:"account_id"`
	Title      string  `json:"title"`
	CategoryID *string `json:"category_id"`
}

// Add records a reviewed message as a transaction. The user chooses the account here: it is never assigned for them.
func (s *Service) Add(ctx context.Context, userID, key, id string, in AddInput) (Event, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Event{}, err
	}
	aid := ids.Normalize(in.AccountID)
	if !ids.Valid(aid) {
		return Event{}, apierr.Validation(map[string]string{"account_id": "Choose the account this message is about."})
	}
	var out Event
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "sms.add", idempotency.Hash("sms.add", []any{id, in}))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM sms_events WHERE id=$1 AND user_id=$2`, replay.ResourceID, userID))
			return err
		}
		e, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM sms_events WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, userID))
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if e.Status != "review" && e.Status != "duplicate" {
			return ErrState
		}
		acct, err := accounts.Fetch(ctx, tx, userID, aid)
		if err != nil || acct.ArchivedAt != nil || acct.Currency != e.Currency {
			return apierr.Validation(map[string]string{"account_id": "Choose one of your active accounts in " + e.Currency + "."})
		}
		typ := "expense"
		if e.Direction == "credit" {
			typ = "income"
		}
		title := strings.TrimSpace(in.Title)
		if title == "" {
			title = e.Merchant
		}
		if title == "" {
			title = "Bank message"
		}
		ref := "sms:fp:" + id
		if e.Reference != "" {
			ref = "sms:ref:" + e.Reference
		}
		txID, err := s.txs.CreateInTx(ctx, tx, userID, transactions.NewTx{AccountID: aid, Type: typ, Amount: e.Amount, Title: title, CategoryID: in.CategoryID, Status: "confirmed",
			OccurredAt: e.ReceivedAt, Timezone: "Asia/Kolkata", Source: "sms", ExternalRef: ref, PayeeName: e.Merchant})
		if err != nil {
			return err
		}
		out, err = scan(tx.QueryRow(ctx, `UPDATE sms_events SET status='added', account_id=$3, transaction_id=$4 WHERE id=$1 AND user_id=$2 RETURNING `+cols, id, userID, aid, txID))
		if err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "sms.added", map[string]any{"event_id": id, "transaction_id": txID}); err != nil {
			return err
		}
		return idempotency.Complete(ctx, tx, userID, key, "sms_event", id)
	})
	return out, err
}

// Link says this message is the same as a transaction the user already has. Nothing is created.
func (s *Service) Link(ctx context.Context, userID, id, transactionID string) (Event, error) {
	tid := ids.Normalize(transactionID)
	if !ids.Valid(tid) {
		return Event{}, apierr.Validation(map[string]string{"transaction_id": "Choose one of your transactions."})
	}
	var one int
	if err := s.db.QueryRow(ctx, `SELECT 1 FROM transactions WHERE id=$1 AND user_id=$2`, tid, userID).Scan(&one); dbx.IsNoRows(err) {
		return Event{}, apierr.Validation(map[string]string{"transaction_id": "Choose one of your transactions."})
	} else if err != nil {
		return Event{}, err
	}
	return s.setStatus(ctx, userID, id, "linked", &tid)
}

func (s *Service) Dismiss(ctx context.Context, userID, id string) (Event, error) {
	return s.setStatus(ctx, userID, id, "dismissed", nil)
}

func (s *Service) setStatus(ctx context.Context, userID, id, status string, txID *string) (Event, error) {
	e, err := scan(s.db.QueryRow(ctx, `UPDATE sms_events SET status=$3, transaction_id=COALESCE($4, transaction_id) WHERE id=$1 AND user_id=$2 AND status IN ('review','duplicate') RETURNING `+cols, id, userID, status, txID))
	if dbx.IsNoRows(err) {
		if _, gerr := scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM sms_events WHERE id=$1 AND user_id=$2`, id, userID)); gerr != nil {
			return Event{}, apierr.ErrNotFound
		}
		return Event{}, ErrState
	}
	return e, err
}

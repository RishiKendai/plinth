// Package transactions owns transactions, transfers, status transitions, history and search.
// Every creation path (manual, import, SMS, recurrence, reconcile) ends in insert(), so there is exactly one
// implementation of the financial rules (CLAUDE.md 3.4).
package transactions

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"plinth/server/internal/platform/apierr"
)

const maxAmount = 1_000_000_000_000_000

type Transaction struct {
	ID                  string     `json:"id"`
	AccountID           string     `json:"account_id"`
	Type                string     `json:"type"` // expense | income | transfer
	Amount              int64      `json:"amount"`
	Currency            string     `json:"currency"`
	Title               string     `json:"title"`
	Description         string     `json:"description"`
	CategoryID          *string    `json:"category_id"`
	PaymentMethod       string     `json:"payment_method"`
	PayerName           string     `json:"payer_name"`
	PayeeName           string     `json:"payee_name"`
	Status              string     `json:"status"`
	OccurredAt          time.Time  `json:"occurred_at"`
	PostedAt            *time.Time `json:"posted_at"`
	Timezone            string     `json:"timezone"`
	Notes               string     `json:"notes"`
	Location            string     `json:"location"`
	Latitude            *float64   `json:"latitude"`
	Longitude           *float64   `json:"longitude"`
	Source              string     `json:"source"`
	ExternalRef         string     `json:"external_ref"`
	TransferID          *string    `json:"transfer_id"`
	TransferDirection   *string    `json:"transfer_direction"`
	CounterpartyAccount *string    `json:"counterparty_account_id"`
	RefundOf            *string    `json:"refund_of"`
	ReplacesID          *string    `json:"replaces_id"`
	ReplacedBy          *string    `json:"replaced_by"`
	Tags                []string   `json:"tags"`
	// CountedAmount is the part that counts as the user's own spending (budgets, trackers). Null means all of it.
	CountedAmount *int64 `json:"counted_amount"`
	// HistoryOnly means no ledger entry exists: the transaction does not change the account balance (docs/balance-rules.md).
	HistoryOnly bool      `json:"history_only"`
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// DeletedAt is set when a voided transaction was deleted (hidden). Only sync returns such rows, so devices can drop them.
	DeletedAt *time.Time `json:"deleted_at"`
}

// Signed returns the effect on the account: negative leaves it.
func (t Transaction) Signed() int64 { return signed(t.Type, t.TransferDirection, t.Amount) }

func signed(typ string, dir *string, amount int64) int64 {
	switch {
	case typ == "expense", typ == "transfer" && dir != nil && *dir == "out":
		return -amount
	default:
		return amount
	}
}

var (
	paymentMethods = map[string]bool{"": true, "cash": true, "upi": true, "debit_card": true, "credit_card": true,
		"bank_transfer": true, "netbanking": true, "wallet": true, "cheque": true, "other": true}
	apiSources      = map[string]bool{"manual": true, "upi": true, "sms": true, "csv": true, "receipt": true}
	initialStatuses = map[string]bool{"confirmed": true, "pending": true, "needs_review": true}

	// Allowed status transitions (docs/balance-rules.md). Anything not listed is rejected.
	transitions = map[string]map[string]bool{
		"pending":      {"confirmed": true, "failed": true, "cancelled": true},
		"needs_review": {"pending": true, "confirmed": true, "cancelled": true},
		"confirmed":    {"reversed": true},
	}
)

var (
	ErrVersion        = apierr.New(http.StatusConflict, "version_conflict", "This transaction changed since you loaded it. Reload and try again.")
	ErrDuplicateEvent = apierr.New(http.StatusConflict, "duplicate_event",
		"That payment is already recorded. Nothing was added.")
	ErrClosed = apierr.New(http.StatusConflict, "transaction_closed",
		"This transaction is closed and cannot be edited. Its history is kept.")
	ErrHasRefunds = apierr.New(http.StatusConflict, "has_refunds",
		"This payment has refunds recorded against it. Void the refunds first.")
	ErrNotRestorable = apierr.New(http.StatusConflict, "not_restorable",
		"This transaction cannot be restored. Record it again instead.")
	ErrNotDeletable = apierr.New(http.StatusConflict, "not_deletable",
		"Only a voided or failed transaction can be deleted, and not one that is part of a correction, a share or a group.")
	ErrCurrency = apierr.Validation(map[string]string{"currency": "Both accounts must use the same currency. Cross-currency transfers are not supported yet."})
)

func transitionErr(from, to string) *apierr.Error {
	return apierr.New(http.StatusConflict, "invalid_transition",
		"A "+strings.ReplaceAll(from, "_", " ")+" transaction cannot become "+strings.ReplaceAll(to, "_", " ")+". Nothing was changed.")
}

// NewTx is a fully resolved transaction ready to be written. All optional values already have defaults.
type NewTx struct {
	ID            string
	AccountID     string
	Type          string
	Amount        int64
	Title         string
	Description   string
	CategoryID    *string
	PaymentMethod string
	PayerName     string
	PayeeName     string
	Status        string
	OccurredAt    time.Time
	PostedAt      *time.Time
	Timezone      string
	Notes         string
	Location      string
	Latitude      *float64
	Longitude     *float64
	Source        string
	ExternalRef   string
	TransferID    *string
	TransferDir   *string
	RefundOf      *string
	ReplacesID    *string
	Tags          []string
	CountedAmount *int64 // see Transaction.CountedAmount; internal modules only
	HistoryOnly   bool   // record without touching the balance; see Transaction.HistoryOnly
}

func trim(s string) string { return strings.TrimSpace(s) }

func runes(s string) int { return utf8.RuneCountInString(s) }

// validate checks the text and number fields. It returns field messages in the form the API reports them.
func (n *NewTx) validate() map[string]string {
	f := map[string]string{}
	n.Title, n.Description, n.Notes = trim(n.Title), trim(n.Description), trim(n.Notes)
	n.PayerName, n.PayeeName, n.Location = trim(n.PayerName), trim(n.PayeeName), trim(n.Location)
	n.ExternalRef = trim(n.ExternalRef)
	if c := runes(n.Title); c < 1 || c > 120 {
		f["title"] = "Enter a title of 1 to 120 characters."
	}
	if runes(n.Description) > 500 {
		f["description"] = "Use 500 characters or fewer."
	}
	if runes(n.Notes) > 2000 {
		f["notes"] = "Use 2,000 characters or fewer."
	}
	if runes(n.PayerName) > 120 {
		f["payer_name"] = "Use 120 characters or fewer."
	}
	if runes(n.PayeeName) > 120 {
		f["payee_name"] = "Use 120 characters or fewer."
	}
	if runes(n.Location) > 200 {
		f["location"] = "Use 200 characters or fewer."
	}
	if runes(n.ExternalRef) > 120 {
		f["external_ref"] = "Use 120 characters or fewer."
	}
	if n.Amount <= 0 {
		f["amount"] = "Enter an amount greater than zero."
	} else if n.Amount > maxAmount {
		f["amount"] = "That amount is too large."
	}
	if n.CountedAmount != nil && (*n.CountedAmount < 0 || *n.CountedAmount > n.Amount) {
		f["counted_amount"] = "The counted amount must be between zero and the amount."
	}
	if !paymentMethods[n.PaymentMethod] {
		f["payment_method"] = "Choose a valid payment method."
	}
	if (n.Latitude == nil) != (n.Longitude == nil) {
		f["latitude"] = "Send both latitude and longitude, or neither."
	} else if n.Latitude != nil && (*n.Latitude < -90 || *n.Latitude > 90 || *n.Longitude < -180 || *n.Longitude > 180) {
		f["latitude"] = "Location is out of range."
	}
	if _, err := time.LoadLocation(n.Timezone); err != nil || n.Timezone == "" || n.Timezone == "Local" {
		f["timezone"] = "Choose a valid time zone, for example Asia/Kolkata."
	}
	tags, tf := cleanTags(n.Tags)
	n.Tags = tags
	if tf != "" {
		f["tags"] = tf
	}
	return f
}

func cleanTags(in []string) ([]string, string) {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range in {
		t = trim(t)
		if t == "" {
			continue
		}
		if runes(t) > 40 {
			return nil, "Each tag can be up to 40 characters."
		}
		k := strings.ToLower(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	if len(out) > 10 {
		return nil, "Use at most 10 tags."
	}
	return out, ""
}

// Package accounts owns financial accounts (assets and liabilities). A financial account is not a user account
// (CLAUDE.md 3.1). The balance is a cache written only by internal/ledger; this package never changes it.
package accounts

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/idempotency"
	"plinth/server/internal/money"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
	"plinth/server/internal/settings"
)

const maxAmount = 1_000_000_000_000_000 // 10^15 minor units, far below int64 overflow when summed

var accountTypes = map[string]bool{"savings": true, "current": true, "cash": true, "credit_card": true, "wallet": true, "fixed_deposit": true, "other": true}

type Account struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Type              string     `json:"type"`
	Currency          string     `json:"currency"`
	OpeningBalance    int64      `json:"opening_balance"`
	OpeningBalanceOn  string     `json:"opening_balance_date"`
	Institution       string     `json:"institution"`
	MaskedIdentifier  string     `json:"masked_identifier"`
	Balance           int64      `json:"balance"`        // opening + pending and confirmed movements
	PendingEffect     int64      `json:"pending_effect"` // the part of balance that comes from pending transactions
	IsLiability       bool       `json:"is_liability"`
	ReconciledAt      *time.Time `json:"reconciled_at"`
	ReconciledBalance *int64     `json:"reconciled_balance"`
	ArchivedAt        *time.Time `json:"archived_at"`
	Version           int        `json:"version"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type CreateInput struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Type             string  `json:"type"`
	Currency         string  `json:"currency"`
	OpeningBalance   int64   `json:"opening_balance"`
	OpeningBalanceOn *string `json:"opening_balance_date"`
	Institution      string  `json:"institution"`
	MaskedIdentifier string  `json:"masked_identifier"`
}

type UpdateInput struct {
	Name             *string `json:"name"`
	Type             *string `json:"type"`
	Institution      *string `json:"institution"`
	MaskedIdentifier *string `json:"masked_identifier"`
	Version          int     `json:"version"`
}

type Summary struct {
	Currency    string `json:"currency"`
	Assets      int64  `json:"assets"`
	Liabilities int64  `json:"liabilities"`
	Net         int64  `json:"net"`
	Accounts    int    `json:"account_count"`
}

var (
	ErrExists       = apierr.New(http.StatusConflict, "account_exists", "You already have an active account with that name.")
	ErrIDTaken      = apierr.New(http.StatusConflict, "id_conflict", "That identifier cannot be used. Generate a new one.")
	ErrVersion      = apierr.New(http.StatusConflict, "version_conflict", "This account changed since you loaded it. Reload and try again.")
	ErrNotEmpty     = apierr.New(http.StatusConflict, "account_not_empty", "The balance must be zero before you archive an account. Transfer the money out or reconcile it first.")
	ErrHasPending   = apierr.New(http.StatusConflict, "account_has_open_transactions", "Resolve pending or needs-review transactions on this account before archiving it.")
	ErrNotArchived  = apierr.New(http.StatusConflict, "account_not_archived", "This account is not archived.")
	ErrAlreadyArch  = apierr.New(http.StatusConflict, "account_archived", "This account is already archived.")
	errNameRequired = "Enter a name of 1 to 80 characters."
)

type Service struct {
	db       *pgxpool.Pool
	settings *settings.Service
	now      func() time.Time
}

func NewService(db *pgxpool.Pool, st *settings.Service) *Service {
	return &Service{db: db, settings: st, now: time.Now}
}

const cols = `a.id::text, a.name, a.type, a.currency, a.opening_balance, to_char(a.opening_balance_date,'YYYY-MM-DD'), a.institution,
	a.masked_identifier, a.balance,
	COALESCE((SELECT SUM(le.amount) FROM ledger_entries le JOIN transactions t ON t.id = le.transaction_id
	          WHERE le.account_id = a.id AND t.status = 'pending'), 0),
	a.type = 'credit_card', a.reconciled_at, a.reconciled_balance, a.archived_at, a.version, a.created_at, a.updated_at`

func scan(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Name, &a.Type, &a.Currency, &a.OpeningBalance, &a.OpeningBalanceOn, &a.Institution,
		&a.MaskedIdentifier, &a.Balance, &a.PendingEffect, &a.IsLiability, &a.ReconciledAt, &a.ReconciledBalance, &a.ArchivedAt,
		&a.Version, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func validName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	return s, n >= 1 && n <= 80
}

func validMask(s string) bool {
	if len(s) > 4 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// Create makes an account. The opening balance is the only balance an account is ever given directly; every
// later change comes from a ledger entry.
func (s *Service) Create(ctx context.Context, userID, key string, in CreateInput) (Account, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Account{}, err
	}
	f := map[string]string{}
	name, ok := validName(in.Name)
	if !ok {
		f["name"] = errNameRequired
	}
	if !accountTypes[in.Type] {
		f["type"] = "Choose an account type."
	}
	in.Institution = strings.TrimSpace(in.Institution)
	if utf8.RuneCountInString(in.Institution) > 80 {
		f["institution"] = "Use 80 characters or fewer."
	}
	if !validMask(in.MaskedIdentifier) {
		f["masked_identifier"] = "Use only the last 4 letters or digits of the account number."
	}
	if in.OpeningBalance > maxAmount || in.OpeningBalance < -maxAmount {
		f["opening_balance"] = "That amount is too large."
	}
	st, err := s.settings.Get(ctx, userID)
	if err != nil {
		return Account{}, err
	}
	if in.Currency == "" {
		in.Currency = st.Currency
	}
	if _, ok := money.Lookup(in.Currency); !ok {
		f["currency"] = "Choose a supported currency."
	}
	loc, _ := time.LoadLocation(st.Timezone)
	if loc == nil {
		loc = time.UTC
	}
	date := s.now().In(loc).Format("2006-01-02")
	if in.OpeningBalanceOn != nil {
		if _, err := time.Parse("2006-01-02", *in.OpeningBalanceOn); err != nil {
			f["opening_balance_date"] = "Use a real date in the form YYYY-MM-DD."
		} else {
			date = *in.OpeningBalanceOn
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
		return Account{}, apierr.Validation(f)
	}
	canon := struct {
		In   CreateInput
		Name string
		Date string
	}{in, name, date}
	canon.In.ID = id

	var out Account
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "account.create", idempotency.Hash("account.create", canon))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM accounts a WHERE a.id=$1 AND a.user_id=$2`, replay.ResourceID, userID))
			return err
		}
		out, err = scan(tx.QueryRow(ctx, `WITH ins AS (
			INSERT INTO accounts (id, user_id, name, type, currency, opening_balance, opening_balance_date, institution, masked_identifier, balance)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$6) RETURNING *)
			SELECT `+cols+` FROM ins a`,
			id, userID, name, in.Type, in.Currency, in.OpeningBalance, date, in.Institution, in.MaskedIdentifier))
		if err != nil {
			switch dbx.ConstraintName(err) {
			case "accounts_user_name_key":
				return ErrExists
			case "accounts_pkey":
				return ErrIDTaken
			}
			return err
		}
		return idempotency.Complete(ctx, tx, userID, key, "account", id)
	})
	return out, err
}

func (s *Service) Get(ctx context.Context, userID, id string) (Account, error) {
	a, err := scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM accounts a WHERE a.id=$1 AND a.user_id=$2`, id, userID))
	if dbx.IsNoRows(err) {
		return a, apierr.ErrNotFound
	}
	return a, err
}

func (s *Service) List(ctx context.Context, userID string, includeArchived bool) ([]Account, error) {
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM accounts a WHERE a.user_id=$1 AND ($2 OR a.archived_at IS NULL)
		ORDER BY a.archived_at IS NOT NULL, a.created_at, a.id`, userID, includeArchived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Summary totals active accounts per currency. Cards are liabilities: net = assets - liabilities = sum of all
// balances. Currencies are never added together.
func (s *Service) Summary(ctx context.Context, userID string) ([]Summary, error) {
	rows, err := s.db.Query(ctx, `SELECT currency,
		COALESCE(SUM(balance) FILTER (WHERE type <> 'credit_card'), 0),
		COALESCE(-SUM(balance) FILTER (WHERE type = 'credit_card'), 0),
		SUM(balance), COUNT(*) FROM accounts WHERE user_id=$1 AND archived_at IS NULL GROUP BY currency ORDER BY currency`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var x Summary
		if err := rows.Scan(&x.Currency, &x.Assets, &x.Liabilities, &x.Net, &x.Accounts); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Update changes descriptive fields. Currency and the opening balance never change: fix a wrong balance by
// reconciling, which records a visible adjustment.
func (s *Service) Update(ctx context.Context, userID, id string, in UpdateInput) (Account, error) {
	f := map[string]string{}
	if in.Name != nil {
		n, ok := validName(*in.Name)
		if !ok {
			f["name"] = errNameRequired
		}
		in.Name = &n
	}
	if in.Type != nil && !accountTypes[*in.Type] {
		f["type"] = "Choose an account type."
	}
	if in.Institution != nil {
		v := strings.TrimSpace(*in.Institution)
		if utf8.RuneCountInString(v) > 80 {
			f["institution"] = "Use 80 characters or fewer."
		}
		in.Institution = &v
	}
	if in.MaskedIdentifier != nil && !validMask(*in.MaskedIdentifier) {
		f["masked_identifier"] = "Use only the last 4 letters or digits of the account number."
	}
	if in.Version < 1 {
		f["version"] = "Send the version you loaded."
	}
	if len(f) > 0 {
		return Account{}, apierr.Validation(f)
	}
	var out Account
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var cur int
		var curType string
		err := tx.QueryRow(ctx, `SELECT version, type FROM accounts WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, userID).Scan(&cur, &curType)
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur != in.Version {
			return ErrVersion
		}
		if in.Type != nil && (*in.Type == "credit_card") != (curType == "credit_card") {
			// Moving to or from a credit card flips how the balance is read (liability), so it is not allowed
			// once the account exists.
			return apierr.Validation(map[string]string{"type": "An account cannot be changed to or from a credit card. Create a new account instead."})
		}
		_, err = tx.Exec(ctx, `UPDATE accounts SET name=COALESCE($3,name), type=COALESCE($4,type), institution=COALESCE($5,institution),
			masked_identifier=COALESCE($6,masked_identifier), version=version+1, updated_at=now() WHERE id=$1 AND user_id=$2`,
			id, userID, in.Name, in.Type, in.Institution, in.MaskedIdentifier)
		if err != nil {
			if dbx.ConstraintName(err) == "accounts_user_name_key" {
				return ErrExists
			}
			return err
		}
		out, err = scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM accounts a WHERE a.id=$1`, id))
		if err != nil {
			return err
		}
		return nil
	})
	return out, err
}

// Archive hides an account from pickers and totals. History is kept. It is refused while money remains or
// transactions are still open, so archiving can never hide a balance.
func (s *Service) Archive(ctx context.Context, userID, key, id string) (Account, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Account{}, err
	}
	return s.setArchived(ctx, userID, key, id, true)
}

func (s *Service) Unarchive(ctx context.Context, userID, key, id string) (Account, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Account{}, err
	}
	return s.setArchived(ctx, userID, key, id, false)
}

func (s *Service) setArchived(ctx context.Context, userID, key, id string, archive bool) (Account, error) {
	scope := "account.unarchive"
	if archive {
		scope = "account.archive"
	}
	var out Account
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, scope, idempotency.Hash(scope, id))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM accounts a WHERE a.id=$1 AND a.user_id=$2`, replay.ResourceID, userID))
			return err
		}
		var balance int64
		var archived *time.Time
		err = tx.QueryRow(ctx, `SELECT balance, archived_at FROM accounts WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, userID).Scan(&balance, &archived)
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if archive {
			if archived != nil {
				return ErrAlreadyArch
			}
			if balance != 0 {
				return ErrNotEmpty
			}
			var open bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM transactions WHERE account_id=$1 AND status IN ('pending','needs_review'))`, id).Scan(&open); err != nil {
				return err
			}
			if open {
				return ErrHasPending
			}
		} else if archived == nil {
			return ErrNotArchived
		}
		if _, err := tx.Exec(ctx, `UPDATE accounts SET archived_at = CASE WHEN $3 THEN now() ELSE NULL END, version=version+1, updated_at=now()
			WHERE id=$1 AND user_id=$2`, id, userID, archive); err != nil {
			if dbx.ConstraintName(err) == "accounts_user_name_key" {
				return ErrExists // restoring would clash with an active account of the same name
			}
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "account", id); err != nil {
			return err
		}
		out, err = scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM accounts a WHERE a.id=$1`, id))
		return err
	})
	return out, err
}

// Fetch reads one account using q (a pool or an open transaction). Used by other modules after a ledger write.
func Fetch(ctx context.Context, q dbx.Q, userID, id string) (Account, error) {
	a, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM accounts a WHERE a.id=$1 AND a.user_id=$2`, id, userID))
	if dbx.IsNoRows(err) {
		return a, apierr.ErrNotFound
	}
	return a, err
}

// MarkReconciled records that the user checked the account against their bank at `at` with the stated balance.
// It writes only the reconciliation checkpoint; the balance itself is never touched here.
func MarkReconciled(ctx context.Context, q dbx.Q, userID, id string, balance int64, at time.Time) error {
	_, err := q.Exec(ctx, `UPDATE accounts SET reconciled_at=$3, reconciled_balance=$4, updated_at=now() WHERE id=$1 AND user_id=$2`, id, userID, at, balance)
	return err
}

// Changes returns accounts changed after the cursor for incremental sync (see transactions.Changes). Archived accounts are
// included: archiving is an update, never a delete.
func (s *Service) Changes(ctx context.Context, userID, cur string, limit int, settle time.Duration) ([]Account, string, bool, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var ts, id any
	if cur != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cur)
		parts := strings.SplitN(string(raw), "|", 2)
		if err != nil || len(parts) != 2 || !ids.Valid(parts[1]) {
			return nil, "", false, apierr.New(400, "bad_cursor", "That page marker is not valid. Start again from the first page.")
		}
		t, perr := time.Parse(time.RFC3339Nano, parts[0])
		if perr != nil {
			return nil, "", false, apierr.New(400, "bad_cursor", "That page marker is not valid. Start again from the first page.")
		}
		ts, id = t, parts[1]
	}
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM accounts a WHERE a.user_id=$1 AND ($2::timestamptz IS NULL OR (a.updated_at, a.id) > ($2::timestamptz, $3::uuid))
		AND a.updated_at < now() - $5::interval ORDER BY a.updated_at, a.id LIMIT $4`, userID, ts, id, limit+1, settle.String())
	if err != nil {
		return nil, "", false, err
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, "", false, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, "", false, err
	}
	more := false
	if len(out) > limit {
		out, more = out[:limit], true
	}
	next := cur
	if n := len(out); n > 0 {
		next = base64.RawURLEncoding.EncodeToString([]byte(out[n-1].UpdatedAt.UTC().Format(time.RFC3339Nano) + "|" + out[n-1].ID))
	}
	return out, next, more, nil
}

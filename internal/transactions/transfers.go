package transactions

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"plinth/server/internal/accounts"
	"plinth/server/internal/audit"
	"plinth/server/internal/idempotency"
	"plinth/server/internal/ledger"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
)

type Transfer struct {
	ID            string        `json:"id"`
	FromAccountID string        `json:"from_account_id"`
	ToAccountID   string        `json:"to_account_id"`
	Amount        int64         `json:"amount"`
	Currency      string        `json:"currency"`
	OccurredAt    time.Time     `json:"occurred_at"`
	Notes         string        `json:"notes"`
	Status        string        `json:"status"`
	Legs          []Transaction `json:"legs"` // [out, in]
	CreatedAt     time.Time     `json:"created_at"`
}

type TransferInput struct {
	ID            string     `json:"id"`
	FromAccountID string     `json:"from_account_id"`
	ToAccountID   string     `json:"to_account_id"`
	Amount        int64      `json:"amount"`
	OccurredAt    *time.Time `json:"occurred_at"`
	Timezone      string     `json:"timezone"`
	Notes         string     `json:"notes"`
	Status        string     `json:"status"` // confirmed (default) or pending
}

// CreateTransfer moves value between two of the user's accounts. Both legs are written in one database
// transaction with a shared transfer ID, under row locks on both accounts taken in a fixed order, so a
// transfer is either fully recorded or not recorded at all. A transfer is neither income nor expense.
func (s *Service) CreateTransfer(ctx context.Context, userID, key string, in TransferInput) (Transfer, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Transfer{}, err
	}
	f := map[string]string{}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		f["id"] = "Identifier must be a UUID."
	}
	if id == "" {
		id = ids.New()
	}
	from, to := ids.Normalize(in.FromAccountID), ids.Normalize(in.ToAccountID)
	if !ids.Valid(from) {
		f["from_account_id"] = "Choose one of your accounts."
	}
	if !ids.Valid(to) {
		f["to_account_id"] = "Choose one of your accounts."
	}
	if from == to && ids.Valid(from) {
		f["to_account_id"] = "Choose a different account to move the money to."
	}
	if in.Amount <= 0 {
		f["amount"] = "Enter an amount greater than zero."
	} else if in.Amount > maxAmount {
		f["amount"] = "That amount is too large."
	}
	status := in.Status
	if status == "" {
		status = "confirmed"
	}
	if status != "confirmed" && status != "pending" {
		f["status"] = "A transfer can be confirmed or pending."
	}
	in.Notes = trim(in.Notes)
	if runes(in.Notes) > 2000 {
		f["notes"] = "Use 2,000 characters or fewer."
	}
	tz := in.Timezone
	if tz == "" {
		var err error
		if tz, err = s.userTimezone(ctx, userID); err != nil {
			return Transfer{}, err
		}
	}
	if _, err := time.LoadLocation(tz); err != nil || tz == "Local" {
		f["timezone"] = "Choose a valid time zone, for example Asia/Kolkata."
	}
	if len(f) > 0 {
		return Transfer{}, apierr.Validation(f)
	}
	occurred := s.now().UTC()
	if in.OccurredAt != nil {
		occurred = in.OccurredAt.UTC()
	}
	canon := in
	canon.ID, canon.FromAccountID, canon.ToAccountID = "", from, to

	var out Transfer
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "transfer.create", idempotency.Hash("transfer.create", canon))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = loadTransfer(ctx, tx, userID, replay.ResourceID)
			return err
		}
		locked, err := ledger.LockAccounts(ctx, tx, userID, from, to)
		if err == ledger.ErrAccountNotFound {
			return apierr.Validation(map[string]string{"from_account_id": "Choose one of your accounts.", "to_account_id": "Choose one of your accounts."})
		}
		if err != nil {
			return err
		}
		if locked[from].Archived || locked[to].Archived {
			return ledger.ErrAccountArchived
		}
		if locked[from].Currency != locked[to].Currency {
			return ErrCurrency
		}
		cur := locked[from].Currency
		var fromName, toName string
		if err := tx.QueryRow(ctx, `SELECT name FROM accounts WHERE id=$1`, from).Scan(&fromName); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT name FROM accounts WHERE id=$1`, to).Scan(&toName); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO transfers (id, user_id, from_account_id, to_account_id, amount, currency, occurred_at, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id, userID, from, to, in.Amount, cur, occurred, in.Notes); err != nil {
			if dbx.ConstraintName(err) == "transfers_pkey" {
				return ErrIDTaken
			}
			return err
		}
		mk := func(acct, dir, title string) NewTx {
			d := dir
			t := id
			return NewTx{ID: ids.New(), AccountID: acct, Type: "transfer", Amount: in.Amount, Title: title, Status: status,
				OccurredAt: occurred, Timezone: tz, Notes: in.Notes, Source: "manual", TransferID: &t, TransferDir: &d}
		}
		for _, leg := range []NewTx{mk(from, "out", "Transfer to "+toName), mk(to, "in", "Transfer from "+fromName)} {
			if f := leg.validate(); len(f) > 0 {
				return apierr.Validation(f)
			}
			if err := write(ctx, tx, userID, cur, leg); err != nil {
				return err
			}
		}
		if err := audit.Log(ctx, tx, userID, "transfer.created", map[string]any{"transfer_id": id, "from": from, "to": to}); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "transfer", id); err != nil {
			return err
		}
		out, err = loadTransfer(ctx, tx, userID, id)
		return err
	})
	return out, err
}

func (s *Service) GetTransfer(ctx context.Context, userID, id string) (Transfer, error) {
	return loadTransfer(ctx, s.db, userID, id)
}

func loadTransfer(ctx context.Context, q dbx.Q, userID, id string) (Transfer, error) {
	var t Transfer
	err := q.QueryRow(ctx, `SELECT id::text, from_account_id::text, to_account_id::text, amount, currency, occurred_at, notes, created_at
		FROM transfers WHERE id=$1 AND user_id=$2`, id, userID).Scan(&t.ID, &t.FromAccountID, &t.ToAccountID, &t.Amount, &t.Currency, &t.OccurredAt, &t.Notes, &t.CreatedAt)
	if dbx.IsNoRows(err) {
		return t, apierr.ErrNotFound
	}
	if err != nil {
		return t, err
	}
	t.OccurredAt, t.CreatedAt = t.OccurredAt.UTC(), t.CreatedAt.UTC()
	rows, err := q.Query(ctx, `SELECT `+txCols+` FROM transactions t WHERE t.transfer_id=$1 AND t.user_id=$2 AND t.deleted_at IS NULL ORDER BY t.transfer_direction DESC`, id, userID)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		l, err := scanTx(rows)
		if err != nil {
			return t, err
		}
		t.Legs = append(t.Legs, l)
	}
	if len(t.Legs) > 0 {
		t.Status = t.Legs[0].Status
	}
	return t, rows.Err()
}

// VoidTransfer cancels or reverses both legs of a transfer in one database transaction.
func (s *Service) VoidTransfer(ctx context.Context, userID, key, id string) (Transfer, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Transfer{}, err
	}
	var legID string
	err := s.db.QueryRow(ctx, `SELECT id::text FROM transactions WHERE transfer_id=$1 AND user_id=$2 AND deleted_at IS NULL ORDER BY id LIMIT 1`, id, userID).Scan(&legID)
	if dbx.IsNoRows(err) {
		return Transfer{}, apierr.ErrNotFound
	}
	if err != nil {
		return Transfer{}, err
	}
	if _, err := s.Void(ctx, userID, key, legID); err != nil {
		return Transfer{}, err
	}
	return s.GetTransfer(ctx, userID, id)
}

// ReconcileResult is what reconciling an account produced.
type ReconcileResult struct {
	Account    accounts.Account `json:"account"`
	Difference int64            `json:"difference"` // actual minus Plinth's balance before reconciling
	Adjustment *Transaction     `json:"adjustment"` // nil when the balances already matched
}

// Reconcile compares the balance the user sees at their bank with Plinth's. A gap is recorded as an explicit,
// visible "Balance adjustment" transaction for exactly the difference (owner decision); the balance is never
// overwritten. A reconciliation checkpoint is stored either way.
func (s *Service) Reconcile(ctx context.Context, userID, key, accountID string, actual int64) (ReconcileResult, error) {
	if err := idempotency.Check(key, true); err != nil {
		return ReconcileResult{}, err
	}
	if actual > maxAmount || actual < -maxAmount {
		return ReconcileResult{}, apierr.Validation(map[string]string{"actual_balance": "That amount is too large."})
	}
	tz, err := s.userTimezone(ctx, userID)
	if err != nil {
		return ReconcileResult{}, err
	}
	var res ReconcileResult
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "account.reconcile", idempotency.Hash("account.reconcile", []any{accountID, actual}))
		if err != nil {
			return err
		}
		if replay != nil {
			if replay.ResourceType == "transaction" {
				adj, err := load(ctx, tx, userID, replay.ResourceID)
				if err != nil {
					return err
				}
				res.Adjustment = &adj
				res.Difference = adj.Signed()
				res.Account, err = accounts.Fetch(ctx, tx, userID, adj.AccountID)
				return err
			}
			res.Account, err = accounts.Fetch(ctx, tx, userID, replay.ResourceID)
			return err
		}
		acct, err := ledger.LockAccount(ctx, tx, userID, accountID)
		if err != nil {
			return err
		}
		if acct.Archived {
			return ledger.ErrAccountArchived
		}
		res.Difference = actual - acct.Balance
		resource, resourceID := "account", accountID
		if res.Difference != 0 {
			n := NewTx{AccountID: accountID, Type: "income", Amount: res.Difference, Title: "Balance adjustment", Status: "confirmed",
				OccurredAt: s.now().UTC(), Timezone: tz, Source: "reconcile",
				Notes: "Recorded when you reconciled this account."}
			if res.Difference < 0 {
				n.Type, n.Amount = "expense", -res.Difference
			}
			id, err := s.CreateInTx(ctx, tx, userID, n)
			if err != nil {
				return err
			}
			adj, err := load(ctx, tx, userID, id)
			if err != nil {
				return err
			}
			res.Adjustment = &adj
			resource, resourceID = "transaction", id
		}
		if err := accounts.MarkReconciled(ctx, tx, userID, accountID, actual, s.now().UTC()); err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "account.reconciled", map[string]any{"account_id": accountID, "difference": res.Difference}); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, resource, resourceID); err != nil {
			return err
		}
		res.Account, err = accounts.Fetch(ctx, tx, userID, accountID)
		return err
	})
	return res, err
}

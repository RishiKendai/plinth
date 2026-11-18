package transactions

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/audit"
	"plinth/server/internal/categories"
	"plinth/server/internal/idempotency"
	"plinth/server/internal/ledger"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
	"plinth/server/internal/settings"
)

var ErrIDTaken = apierr.New(http.StatusConflict, "id_conflict", "That identifier cannot be used. Generate a new one.")

type Service struct {
	db       *pgxpool.Pool
	settings *settings.Service
	now      func() time.Time
}

func NewService(db *pgxpool.Pool, st *settings.Service) *Service {
	return &Service{db: db, settings: st, now: time.Now}
}

const txCols = `t.id::text, t.account_id::text, t.type, t.amount, t.currency, t.title, t.description, t.category_id::text,
	t.payment_method, t.payer_name, t.payee_name, t.status, t.occurred_at, t.posted_at, t.timezone, t.notes, t.location,
	t.latitude::float8, t.longitude::float8, t.source, t.external_ref, t.transfer_id::text, t.transfer_direction,
	CASE WHEN t.transfer_id IS NULL THEN NULL ELSE (SELECT o.account_id::text FROM transactions o WHERE o.transfer_id = t.transfer_id AND o.id <> t.id LIMIT 1) END,
	t.refund_of::text, t.replaces_id::text, (SELECT r.id::text FROM transactions r WHERE r.replaces_id = t.id LIMIT 1),
	COALESCE((SELECT array_agg(tg.name ORDER BY lower(tg.name)) FROM transaction_tags tt JOIN tags tg ON tg.id = tt.tag_id WHERE tt.transaction_id = t.id), '{}'::text[]),
	t.counted_amount, t.version, t.created_at, t.updated_at`

func scanTx(row interface{ Scan(...any) error }) (Transaction, error) {
	var t Transaction
	err := row.Scan(&t.ID, &t.AccountID, &t.Type, &t.Amount, &t.Currency, &t.Title, &t.Description, &t.CategoryID,
		&t.PaymentMethod, &t.PayerName, &t.PayeeName, &t.Status, &t.OccurredAt, &t.PostedAt, &t.Timezone, &t.Notes, &t.Location,
		&t.Latitude, &t.Longitude, &t.Source, &t.ExternalRef, &t.TransferID, &t.TransferDirection, &t.CounterpartyAccount,
		&t.RefundOf, &t.ReplacesID, &t.ReplacedBy, &t.Tags, &t.CountedAmount, &t.Version, &t.CreatedAt, &t.UpdatedAt)
	t.OccurredAt = t.OccurredAt.UTC()
	t.CreatedAt, t.UpdatedAt = t.CreatedAt.UTC(), t.UpdatedAt.UTC()
	if t.PostedAt != nil {
		u := t.PostedAt.UTC()
		t.PostedAt = &u
	}
	return t, err
}

// load reads one transaction owned by userID.
func load(ctx context.Context, q dbx.Q, userID, id string) (Transaction, error) {
	t, err := scanTx(q.QueryRow(ctx, `SELECT `+txCols+` FROM transactions t WHERE t.id=$1 AND t.user_id=$2`, id, userID))
	if dbx.IsNoRows(err) {
		return t, apierr.ErrNotFound
	}
	return t, err
}

func (s *Service) Get(ctx context.Context, userID, id string) (Transaction, error) {
	return load(ctx, s.db, userID, id)
}

// prepare runs the checks every new or edited transaction must pass, with the account locked: ownership, not
// archived, category kind, refund limits. It returns the account currency, which the transaction inherits.
// excludeID is the transaction being replaced or edited, so it is not counted against itself.
// allowArchived lets a descriptive edit touch history on an account archived since.
func prepare(ctx context.Context, tx pgx.Tx, userID string, n *NewTx, excludeID string, allowArchived bool) (string, error) {
	f := map[string]string{}
	if !ids.Valid(n.AccountID) {
		return "", apierr.Validation(map[string]string{"account_id": "Choose one of your accounts."})
	}
	acct, err := ledger.LockAccount(ctx, tx, userID, n.AccountID)
	if err == ledger.ErrAccountNotFound {
		return "", apierr.Validation(map[string]string{"account_id": "Choose one of your accounts."})
	}
	if err != nil {
		return "", err
	}
	if acct.Archived && !allowArchived {
		return "", ledger.ErrAccountArchived
	}
	if n.Type == "transfer" {
		n.CategoryID = nil
	} else if n.CategoryID != nil {
		cid := ids.Normalize(*n.CategoryID)
		n.CategoryID = &cid
		if !ids.Valid(cid) {
			f["category_id"] = "Choose one of your categories."
		} else if c, err := categories.Visible(ctx, tx, userID, cid); err != nil {
			f["category_id"] = "Choose one of your categories."
		} else if c.Kind != n.Type {
			f["category_id"] = "That category is for " + c.Kind + "s. Choose a " + n.Type + " category."
		}
	}
	if n.RefundOf != nil {
		rid := ids.Normalize(*n.RefundOf)
		n.RefundOf = &rid
		if n.Type != "income" {
			f["refund_of"] = "Only income can be a refund."
		} else if !ids.Valid(rid) {
			f["refund_of"] = "Choose the payment this refunds."
		} else {
			var typ, cur, st string
			var amt int64
			err := tx.QueryRow(ctx, `SELECT type, currency, status, amount FROM transactions WHERE id=$1 AND user_id=$2 FOR UPDATE`, rid, userID).Scan(&typ, &cur, &st, &amt)
			switch {
			case dbx.IsNoRows(err), err == nil && (typ != "expense" || !ledger.Counting(st)):
				f["refund_of"] = "Choose a recorded expense to refund."
			case err != nil:
				return "", err
			case cur != acct.Currency:
				f["refund_of"] = "A refund must be in the same currency as the payment."
			default:
				var refunded int64
				if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0) FROM transactions WHERE refund_of=$1 AND status IN ('pending','confirmed') AND ($2::uuid IS NULL OR id <> $2::uuid)`,
					rid, nullUUID(excludeID)).Scan(&refunded); err != nil {
					return "", err
				}
				if refunded+n.Amount > amt {
					f["amount"] = "The refund is more than the original payment."
				}
			}
		}
	}
	if len(f) > 0 {
		return "", apierr.Validation(f)
	}
	return acct.Currency, nil
}

// nullUUID lets an empty string stand for "no transaction" in a uuid comparison.
func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// write inserts the row, its tags and (for counting statuses) the ledger entry. prepare must have succeeded.
func write(ctx context.Context, tx pgx.Tx, userID, currency string, n NewTx) error {
	posted := n.PostedAt
	if posted == nil && n.Status == "confirmed" {
		t := time.Now().UTC()
		posted = &t
	}
	_, err := tx.Exec(ctx, `INSERT INTO transactions (id, user_id, account_id, type, amount, currency, title, description, category_id,
		payment_method, payer_name, payee_name, status, occurred_at, posted_at, timezone, notes, location, latitude, longitude,
		source, external_ref, transfer_id, transfer_direction, refund_of, replaces_id, counted_amount)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
		n.ID, userID, n.AccountID, n.Type, n.Amount, currency, n.Title, n.Description, n.CategoryID,
		n.PaymentMethod, n.PayerName, n.PayeeName, n.Status, n.OccurredAt, posted, n.Timezone, n.Notes, n.Location, n.Latitude, n.Longitude,
		n.Source, n.ExternalRef, n.TransferID, n.TransferDir, n.RefundOf, n.ReplacesID, n.CountedAmount)
	if err != nil {
		switch dbx.ConstraintName(err) {
		case "transactions_source_event_key":
			return ErrDuplicateEvent
		case "transactions_pkey":
			return ErrIDTaken
		}
		return err
	}
	if err := setTags(ctx, tx, userID, n.ID, n.Tags); err != nil {
		return err
	}
	if ledger.Counting(n.Status) {
		return ledger.Post(ctx, tx, userID, n.AccountID, n.ID, currency, signed(n.Type, n.TransferDir, n.Amount), n.OccurredAt, "movement")
	}
	return nil
}

func setTags(ctx context.Context, tx pgx.Tx, userID, txID string, names []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM transaction_tags WHERE transaction_id=$1`, txID); err != nil {
		return err
	}
	for _, name := range names {
		var tagID string
		if err := tx.QueryRow(ctx, `INSERT INTO tags (user_id, name) VALUES ($1,$2)
			ON CONFLICT (user_id, lower(name)) DO UPDATE SET name = tags.name RETURNING id::text`, userID, name).Scan(&tagID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO transaction_tags (transaction_id, tag_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, txID, tagID); err != nil {
			return err
		}
	}
	return nil
}

// CreateInTx validates and records a transaction inside the caller's database transaction. Other modules
// (recurrence, reminders, imports) use it so the financial rules live in one place. It returns the new ID.
func (s *Service) CreateInTx(ctx context.Context, tx pgx.Tx, userID string, n NewTx) (string, error) {
	if n.ID == "" {
		n.ID = ids.New()
	}
	if n.Status == "" {
		n.Status = "confirmed"
	}
	if n.Type != "expense" && n.Type != "income" {
		return "", apierr.Validation(map[string]string{"type": "Choose expense or income."})
	}
	if f := n.validate(); len(f) > 0 {
		return "", apierr.Validation(f)
	}
	cur, err := prepare(ctx, tx, userID, &n, "", false)
	if err != nil {
		return "", err
	}
	if err := write(ctx, tx, userID, cur, n); err != nil {
		return "", err
	}
	return n.ID, audit.Log(ctx, tx, userID, "transaction.created", map[string]any{"transaction_id": n.ID, "source": n.Source, "type": n.Type})
}

type CreateInput struct {
	ID            string     `json:"id"`
	AccountID     string     `json:"account_id"`
	Type          string     `json:"type"`
	Amount        int64      `json:"amount"`
	Title         string     `json:"title"`
	Description   string     `json:"description"`
	CategoryID    *string    `json:"category_id"`
	PaymentMethod string     `json:"payment_method"`
	PayerName     string     `json:"payer_name"`
	PayeeName     string     `json:"payee_name"`
	Status        string     `json:"status"`
	OccurredAt    *time.Time `json:"occurred_at"`
	Timezone      string     `json:"timezone"`
	Notes         string     `json:"notes"`
	Location      string     `json:"location"`
	Latitude      *float64   `json:"latitude"`
	Longitude     *float64   `json:"longitude"`
	Source        string     `json:"source"`
	ExternalRef   string     `json:"external_ref"`
	RefundOf      *string    `json:"refund_of"`
	Tags          []string   `json:"tags"`
}

func (s *Service) userTimezone(ctx context.Context, userID string) (string, error) {
	st, err := s.settings.Get(ctx, userID)
	return st.Timezone, err
}

func (s *Service) Create(ctx context.Context, userID, key string, in CreateInput) (Transaction, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Transaction{}, err
	}
	f := map[string]string{}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		f["id"] = "Identifier must be a UUID."
	}
	if id == "" {
		id = ids.New()
	}
	if in.Type != "expense" && in.Type != "income" {
		f["type"] = "Choose expense or income. Use the transfer form to move money between accounts."
	}
	status := in.Status
	if status == "" {
		status = "confirmed"
	}
	if !initialStatuses[status] {
		f["status"] = "A new transaction can be confirmed, pending or needs review."
	}
	source := in.Source
	if source == "" {
		source = "manual"
	}
	if !apiSources[source] {
		f["source"] = "Choose a valid source."
	}
	tz := in.Timezone
	if tz == "" {
		var err error
		if tz, err = s.userTimezone(ctx, userID); err != nil {
			return Transaction{}, err
		}
	}
	occurred := s.now().UTC()
	if in.OccurredAt != nil {
		occurred = in.OccurredAt.UTC()
	}
	n := NewTx{ID: id, AccountID: ids.Normalize(in.AccountID), Type: in.Type, Amount: in.Amount, Title: in.Title, Description: in.Description,
		CategoryID: in.CategoryID, PaymentMethod: in.PaymentMethod, PayerName: in.PayerName, PayeeName: in.PayeeName, Status: status,
		OccurredAt: occurred, Timezone: tz, Notes: in.Notes, Location: in.Location, Latitude: in.Latitude, Longitude: in.Longitude,
		Source: source, ExternalRef: in.ExternalRef, RefundOf: in.RefundOf, Tags: in.Tags}
	for k, v := range n.validate() {
		f[k] = v
	}
	if len(f) > 0 {
		return Transaction{}, apierr.Validation(f)
	}
	// Fingerprint the request as sent, not as defaulted: a retry has a different "now" and generated ID.
	canon := in
	canon.ID = ""
	canon.AccountID = n.AccountID
	var out Transaction
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "transaction.create", idempotency.Hash("transaction.create", canon))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = load(ctx, tx, userID, replay.ResourceID)
			return err
		}
		cur, err := prepare(ctx, tx, userID, &n, "", false)
		if err != nil {
			return err
		}
		if err := write(ctx, tx, userID, cur, n); err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "transaction.created", map[string]any{"transaction_id": id, "source": source, "type": n.Type}); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "transaction", id); err != nil {
			return err
		}
		out, err = load(ctx, tx, userID, id)
		return err
	})
	return out, err
}

// legs locks and returns the transaction and, for a transfer, its other leg. Rows are locked in ID order.
func legs(ctx context.Context, tx pgx.Tx, userID, id string) ([]Transaction, error) {
	var transferID *string
	err := tx.QueryRow(ctx, `SELECT transfer_id::text FROM transactions WHERE id=$1 AND user_id=$2`, id, userID).Scan(&transferID)
	if dbx.IsNoRows(err) {
		return nil, apierr.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var rows pgx.Rows
	if transferID == nil {
		rows, err = tx.Query(ctx, `SELECT `+txCols+` FROM transactions t WHERE t.id=$1 AND t.user_id=$2 FOR UPDATE OF t`, id, userID)
	} else {
		rows, err = tx.Query(ctx, `SELECT `+txCols+` FROM transactions t WHERE t.transfer_id=$1 AND t.user_id=$2 ORDER BY t.id FOR UPDATE OF t`, *transferID, userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transaction
	for rows.Next() {
		t, err := scanTx(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func lockLegAccounts(ctx context.Context, tx pgx.Tx, userID string, ls []Transaction) error {
	idsToLock := make([]string, len(ls))
	for i, l := range ls {
		idsToLock[i] = l.AccountID
	}
	_, err := ledger.LockAccounts(ctx, tx, userID, idsToLock...)
	return err
}

// applyStatus moves one locked leg to status `to`, keeping the ledger consistent. The leg's account is locked.
func applyStatus(ctx context.Context, tx pgx.Tx, userID string, l Transaction, to string) error {
	if !transitions[l.Status][to] {
		return transitionErr(l.Status, to)
	}
	was, will := ledger.Counting(l.Status), ledger.Counting(to)
	if was && !will && l.Type == "expense" {
		var refunds bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM transactions WHERE refund_of=$1 AND status IN ('pending','confirmed'))`, l.ID).Scan(&refunds); err != nil {
			return err
		}
		if refunds {
			return ErrHasRefunds
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE transactions SET status=$2, version=version+1, updated_at=now(),
		posted_at = CASE WHEN $2 = 'confirmed' THEN COALESCE(posted_at, now()) ELSE posted_at END WHERE id=$1`, l.ID, to); err != nil {
		return err
	}
	switch {
	case was && !will:
		return ledger.Reverse(ctx, tx, userID, l.ID)
	case !was && will:
		return ledger.Post(ctx, tx, userID, l.AccountID, l.ID, l.Currency, l.Signed(), l.OccurredAt, "movement")
	}
	return nil
}

// ChangeStatus moves a transaction (and both legs of a transfer) to a new status using the documented
// transition table. Failed, cancelled and reversed are final.
func (s *Service) ChangeStatus(ctx context.Context, userID, key, id, to string) (Transaction, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Transaction{}, err
	}
	if to == "" || !(to == "pending" || to == "confirmed" || to == "failed" || to == "cancelled" || to == "reversed") {
		return Transaction{}, apierr.Validation(map[string]string{"status": "Choose pending, confirmed, failed, cancelled or reversed."})
	}
	return s.mutate(ctx, userID, key, "transaction.status", id, to, func(tx pgx.Tx, ls []Transaction) error {
		for _, l := range ls {
			if err := applyStatus(ctx, tx, userID, l, to); err != nil {
				return err
			}
		}
		return audit.Log(ctx, tx, userID, "transaction.status_changed", map[string]any{"transaction_id": id, "from": ls[0].Status, "to": to})
	})
}

// Void cancels a pending or needs-review transaction, or reverses a confirmed one. Nothing is deleted:
// the original stays in history and a reversing ledger entry cancels its effect.
func (s *Service) Void(ctx context.Context, userID, key, id string) (Transaction, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Transaction{}, err
	}
	return s.mutate(ctx, userID, key, "transaction.void", id, "", func(tx pgx.Tx, ls []Transaction) error {
		for _, l := range ls {
			to := "cancelled"
			if l.Status == "confirmed" {
				to = "reversed"
			}
			if err := applyStatus(ctx, tx, userID, l, to); err != nil {
				return err
			}
		}
		return audit.Log(ctx, tx, userID, "transaction.voided", map[string]any{"transaction_id": id, "from": ls[0].Status})
	})
}

// mutate runs a status-style change under idempotency with the transaction and its legs locked.
func (s *Service) mutate(ctx context.Context, userID, key, scope, id, arg string, fn func(pgx.Tx, []Transaction) error) (Transaction, error) {
	var out Transaction
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, scope, idempotency.Hash(scope, []string{id, arg}))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = load(ctx, tx, userID, replay.ResourceID)
			return err
		}
		ls, err := legs(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		if err := lockLegAccounts(ctx, tx, userID, ls); err != nil {
			return err
		}
		if err := fn(tx, ls); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "transaction", id); err != nil {
			return err
		}
		out, err = load(ctx, tx, userID, id)
		return err
	})
	return out, err
}

// ownsAccount returns 404 unless the account belongs to the user.
func (s *Service) ownsAccount(ctx context.Context, userID, accountID string) error {
	var one int
	err := s.db.QueryRow(ctx, `SELECT 1 FROM accounts WHERE id=$1 AND user_id=$2`, accountID, userID).Scan(&one)
	if dbx.IsNoRows(err) {
		return apierr.ErrNotFound
	}
	return err
}

// VoidInTx voids a transaction (and both legs of a transfer) inside the caller's database transaction. Other modules
// (CSV import undo, group expenses) use it so reversals follow the same rules. Already-closed transactions are left
// alone, which makes it safe to call twice.
func (s *Service) VoidInTx(ctx context.Context, tx pgx.Tx, userID, id string) error {
	ls, err := legs(ctx, tx, userID, id)
	if err != nil {
		return err
	}
	if err := lockLegAccounts(ctx, tx, userID, ls); err != nil {
		return err
	}
	for _, l := range ls {
		if l.Status == "failed" || l.Status == "cancelled" || l.Status == "reversed" {
			continue
		}
		to := "cancelled"
		if l.Status == "confirmed" {
			to = "reversed"
		}
		if err := applyStatus(ctx, tx, userID, l, to); err != nil {
			return err
		}
	}
	return audit.Log(ctx, tx, userID, "transaction.voided", map[string]any{"transaction_id": id, "by": "system"})
}

// ChangeStatusInTx applies a status transition inside the caller's database transaction. Other modules (UPI outcomes,
// bank-message verification) use it so the transition table and ledger rules stay in one place. It returns the
// transaction after the change.
func (s *Service) ChangeStatusInTx(ctx context.Context, tx pgx.Tx, userID, id, to string) (Transaction, error) {
	ls, err := legs(ctx, tx, userID, id)
	if err != nil {
		return Transaction{}, err
	}
	if err := lockLegAccounts(ctx, tx, userID, ls); err != nil {
		return Transaction{}, err
	}
	for _, l := range ls {
		if err := applyStatus(ctx, tx, userID, l, to); err != nil {
			return Transaction{}, err
		}
	}
	if err := audit.Log(ctx, tx, userID, "transaction.status_changed", map[string]any{"transaction_id": id, "from": ls[0].Status, "to": to}); err != nil {
		return Transaction{}, err
	}
	return load(ctx, tx, userID, id)
}

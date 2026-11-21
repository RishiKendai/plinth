package groups

import (
	"context"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"plinth/server/internal/accounts"
	"plinth/server/internal/idempotency"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
	"plinth/server/internal/transactions"
)

type TopupInput struct {
	ID         string     `json:"id"`
	Amount     int64      `json:"amount"`
	Note       string     `json:"note"`
	OccurredAt *time.Time `json:"occurred_at"`
	// AccountID optionally records the money leaving one of the caller's own accounts (source topup: it moves that balance but is
	// not counted as spending, like a settlement).
	AccountID string `json:"account_id"`
}

type Topup struct {
	ID         string    `json:"id"`
	GroupID    string    `json:"group_id"`
	MemberID   string    `json:"member_id"`
	Amount     int64     `json:"amount"`
	Currency   string    `json:"currency"`
	Note       string    `json:"note"`
	OccurredAt time.Time `json:"occurred_at"`
	Voided     bool      `json:"voided"`
	InMyLedger bool      `json:"in_my_ledger"`
	CreatedAt  time.Time `json:"created_at"`
	txID       *string
}

const topupCols = `t.id::text, t.group_id::text, t.member_id::text, t.amount, g.currency, t.note, t.occurred_at, t.voided_at IS NOT NULL, t.created_at, t.transaction_id::text`

func scanTopup(r pgx.Row) (Topup, error) {
	var t Topup
	err := r.Scan(&t.ID, &t.GroupID, &t.MemberID, &t.Amount, &t.Currency, &t.Note, &t.OccurredAt, &t.Voided, &t.CreatedAt, &t.txID)
	t.OccurredAt, t.CreatedAt = t.OccurredAt.UTC(), t.CreatedAt.UTC()
	return t, err
}

// AddTopup adds money to a budget group. Any member may top up. It is recorded as a transaction of the group; with account_id it
// is also recorded in the caller's own ledger. A spending tracker has no money to top up.
func (s *Service) AddTopup(ctx context.Context, userID, key, groupID string, in TopupInput) (Topup, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Topup{}, err
	}
	f := map[string]string{}
	if in.Amount <= 0 || in.Amount > 1_000_000_000_000_000 {
		f["amount"] = "Enter an amount greater than zero."
	}
	if utf8.RuneCountInString(in.Note) > 500 {
		f["note"] = "Use 500 characters or fewer."
	}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		f["id"] = "Identifier must be a UUID."
	}
	if id == "" {
		id = ids.New()
	}
	if len(f) > 0 {
		return Topup{}, apierr.Validation(f)
	}
	var out Topup
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "group.topup", idempotency.Hash("group.topup", []any{groupID, in}))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = scanTopup(tx.QueryRow(ctx, `SELECT `+topupCols+` FROM group_topups t JOIN groups g ON g.id=t.group_id WHERE t.id=$1`, replay.ResourceID))
			return err
		}
		myID, _, err := me(ctx, tx, userID, groupID)
		if err != nil {
			return err
		}
		cur, archived, err := s.groupMeta(ctx, tx, groupID)
		if err != nil {
			return err
		}
		if archived {
			return ErrArchived
		}
		var purpose, name string
		if err := tx.QueryRow(ctx, `SELECT purpose, name FROM groups WHERE id=$1 FOR UPDATE`, groupID).Scan(&purpose, &name); err != nil {
			return err
		}
		if purpose != "budget" {
			return apierr.New(http.StatusConflict, "not_a_budget", "This group is a spending tracker, so there is no money to top up.")
		}
		when := s.now().UTC()
		if in.OccurredAt != nil {
			when = in.OccurredAt.UTC()
		}
		var txID *string
		if in.AccountID != "" {
			acct, err := accountFor(ctx, tx, userID, ids.Normalize(in.AccountID), cur)
			if err != nil {
				return err
			}
			t, err := s.txs.CreateInTx(ctx, tx, userID, transactions.NewTx{AccountID: acct, Type: "expense", Amount: in.Amount, Title: "Top-up: " + clip(name, 100), Status: "confirmed",
				OccurredAt: when, Timezone: "Asia/Kolkata", Source: "topup", ExternalRef: "topup:" + id, Notes: in.Note})
			if err != nil {
				return err
			}
			txID = &t
		}
		if _, err := tx.Exec(ctx, `INSERT INTO group_topups (id, group_id, member_id, amount, note, occurred_at, transaction_id) VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, groupID, myID, in.Amount, in.Note, when, txID); err != nil {
			if dbx.ConstraintName(err) == "group_topups_pkey" {
				return transactions.ErrIDTaken
			}
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "group_topup", id); err != nil {
			return err
		}
		out, err = s.topup(ctx, tx, userID, id)
		return err
	})
	return out, err
}

func (s *Service) topup(ctx context.Context, q dbx.Q, userID, id string) (Topup, error) {
	t, err := scanTopup(q.QueryRow(ctx, `SELECT `+topupCols+` FROM group_topups t JOIN groups g ON g.id=t.group_id
		JOIN group_members m ON m.group_id=t.group_id AND m.user_id=$2 AND m.removed_at IS NULL WHERE t.id=$1`, id, userID))
	if dbx.IsNoRows(err) {
		return t, apierr.ErrNotFound
	}
	if err != nil {
		return t, err
	}
	if t.txID != nil {
		var own string
		if err := q.QueryRow(ctx, `SELECT user_id::text FROM transactions WHERE id=$1`, *t.txID).Scan(&own); err == nil {
			t.InMyLedger = own == userID
		}
	}
	return t, nil
}

func (s *Service) ListTopups(ctx context.Context, userID, groupID string) ([]Topup, error) {
	if _, _, err := me(ctx, s.db, userID, groupID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT t.id::text FROM group_topups t WHERE t.group_id=$1 ORDER BY t.occurred_at DESC, t.id`, groupID)
	if err != nil {
		return nil, err
	}
	var idList []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		idList = append(idList, id)
	}
	rows.Close()
	out := []Topup{}
	for _, id := range idList {
		t, err := s.topup(ctx, s.db, userID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// VoidTopup takes a top-up back. It is refused if the money has already been spent (the budget would go below zero). The member's own
// ledger transaction is voided only when its owner is the one asking.
func (s *Service) VoidTopup(ctx context.Context, userID, id string) (Topup, error) {
	var out Topup
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		t, err := s.topup(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		myID, role, err := me(ctx, tx, userID, t.GroupID)
		if err != nil {
			return err
		}
		if t.MemberID != myID && role != "admin" {
			return ErrForbidden
		}
		if !t.Voided {
			var remaining int64
			if err := tx.QueryRow(ctx, `SELECT COALESCE(g.budget_limit,0)
				+ COALESCE((SELECT SUM(x.amount) FROM group_topups x WHERE x.group_id=g.id AND x.voided_at IS NULL),0)
				- COALESCE((SELECT SUM(e.amount) FROM group_expenses e WHERE e.group_id=g.id AND e.voided_at IS NULL),0) FROM groups g WHERE g.id=$1 FOR UPDATE`, t.GroupID).Scan(&remaining); err != nil {
				return err
			}
			if remaining-t.Amount < 0 {
				return apierr.New(http.StatusConflict, "topup_spent", "That money has already been spent, so this top-up cannot be taken back.")
			}
			if _, err := tx.Exec(ctx, `UPDATE group_topups SET voided_at=now() WHERE id=$1 AND voided_at IS NULL`, id); err != nil {
				return err
			}
			if t.InMyLedger && t.txID != nil {
				if err := s.voidOwn(ctx, tx, userID, *t.txID); err != nil {
					return err
				}
			}
		}
		out, err = s.topup(ctx, tx, userID, id)
		return err
	})
	return out, err
}

// voidOwn voids a caller's own transaction that a group record created (source group, settlement or topup); one the caller shared
// from an existing transaction simply goes back to counting in full. Already-voided ones are left alone.
func (s *Service) voidOwn(ctx context.Context, tx pgx.Tx, userID, txID string) error {
	var src, status string
	err := tx.QueryRow(ctx, `SELECT source, status FROM transactions WHERE id=$1 AND user_id=$2`, txID, userID).Scan(&src, &status)
	if dbx.IsNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if src == "group" || src == "settlement" || src == "topup" {
		if status == "pending" || status == "confirmed" {
			return s.txs.VoidInTx(ctx, tx, userID, txID)
		}
		return nil
	}
	_, err = tx.Exec(ctx, `UPDATE transactions SET counted_amount=NULL, version=version+1, updated_at=now() WHERE id=$1 AND user_id=$2`, txID, userID)
	return err
}

// DeleteGroup permanently deletes a group (or split) and everything recorded in it: members, expenses, shares, settlements, top-ups
// and invites. Only an admin may do it, and only when no other Plinth user is still a member (removing them needs their balance
// settled), because their personal records are theirs alone. The admin's own ledger transactions that the group created are voided
// (a reversal that restores the balance; ledger history is append-only and is never erased) and transactions shared from an
// existing one count in full again.
func (s *Service) DeleteGroup(ctx context.Context, userID, groupID string) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := admin(ctx, tx, userID, groupID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM groups WHERE id=$1 FOR UPDATE`, groupID); err != nil {
			return err
		}
		var others int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM group_members WHERE group_id=$1 AND removed_at IS NULL AND user_id IS NOT NULL AND user_id <> $2`, groupID, userID).Scan(&others); err != nil {
			return err
		}
		if others > 0 {
			return apierr.New(http.StatusConflict, "has_other_members", "Other people on Plinth are still in this group. Remove them first, then delete it.")
		}
		rows, err := tx.Query(ctx, `SELECT transaction_id::text FROM group_expenses WHERE group_id=$1 AND transaction_id IS NOT NULL
			UNION SELECT transaction_id::text FROM group_settlements WHERE group_id=$1 AND transaction_id IS NOT NULL
			UNION SELECT transaction_id::text FROM group_topups WHERE group_id=$1 AND transaction_id IS NOT NULL`, groupID)
		if err != nil {
			return err
		}
		var txIDs []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			txIDs = append(txIDs, id)
		}
		rows.Close()
		for _, id := range txIDs {
			if err := s.voidOwn(ctx, tx, userID, id); err != nil {
				return err
			}
		}
		// Children first, in dependency order: shares reference members without a cascade of their own.
		for _, q := range []string{
			`DELETE FROM expense_shares WHERE expense_id IN (SELECT id FROM group_expenses WHERE group_id=$1)`,
			`DELETE FROM group_expenses WHERE group_id=$1`, `DELETE FROM group_settlements WHERE group_id=$1`, `DELETE FROM group_topups WHERE group_id=$1`,
			`DELETE FROM groups WHERE id=$1`,
		} {
			if _, err := tx.Exec(ctx, q, groupID); err != nil {
				return err
			}
		}
		return nil
	})
}

// accountFor checks that aid is one of the caller's active accounts in the group's currency and returns it.
func accountFor(ctx context.Context, tx pgx.Tx, userID, aid, groupCurrency string) (string, error) {
	acct, err := accounts.Fetch(ctx, tx, userID, aid)
	if err != nil || acct.ArchivedAt != nil {
		return "", apierr.Validation(map[string]string{"account_id": "Choose one of your active accounts."})
	}
	if acct.Currency != groupCurrency {
		return "", apierr.Validation(map[string]string{"account_id": "That account uses " + acct.Currency + " but the group uses " + groupCurrency + "."})
	}
	return aid, nil
}

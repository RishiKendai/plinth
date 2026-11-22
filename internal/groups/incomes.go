package groups

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
)

// GroupIncome files one of the caller's own income transactions under a group. It is a label only: nothing is split, no member balance,
// spent total or budget changes, and the transaction and account balance are untouched.
type GroupIncome struct {
	ID            string    `json:"id"`
	GroupID       string    `json:"group_id"`
	TransactionID string    `json:"transaction_id"`
	Title         string    `json:"title"`
	Amount        int64     `json:"amount"`
	Currency      string    `json:"currency"`
	OccurredAt    time.Time `json:"occurred_at"`
	Voided        bool      `json:"voided"`
}

type IncomeInput struct {
	TransactionID string `json:"transaction_id"`
	// Move takes the income out of the group it is in now. Without it an income that is already in a group is refused (409 already_shared).
	Move bool `json:"move"`
}

const incomeCols = `i.id::text, i.group_id::text, i.transaction_id::text, i.title, i.amount, i.currency, i.occurred_at, i.voided_at IS NOT NULL`

func scanIncome(row pgx.Row) (GroupIncome, error) {
	var g GroupIncome
	err := row.Scan(&g.ID, &g.GroupID, &g.TransactionID, &g.Title, &g.Amount, &g.Currency, &g.OccurredAt, &g.Voided)
	g.OccurredAt = g.OccurredAt.UTC()
	return g, err
}

func (s *Service) AddIncome(ctx context.Context, userID, groupID string, in IncomeInput) (GroupIncome, error) {
	tid := ids.Normalize(in.TransactionID)
	bad := func(m string) error { return apierr.Validation(map[string]string{"transaction_id": m}) }
	if !ids.Valid(tid) {
		return GroupIncome{}, bad("Choose one of your income transactions.")
	}
	var out GroupIncome
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
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
		var typ, tcur, tstatus, tsource, title string
		var amount int64
		var when time.Time
		err = tx.QueryRow(ctx, `SELECT type, currency, status, source, amount, title, occurred_at FROM transactions WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL FOR UPDATE`, tid, userID).
			Scan(&typ, &tcur, &tstatus, &tsource, &amount, &title, &when)
		switch {
		case dbx.IsNoRows(err):
			return bad("Choose one of your income transactions.")
		case err != nil:
			return err
		case typ != "income" || (tstatus != "pending" && tstatus != "confirmed"):
			return bad("Only a recorded income can be added this way.")
		case tsource == "group" || tsource == "settlement" || tsource == "topup":
			return bad("That was recorded inside a group, so it cannot be added to another one.")
		case tcur != cur:
			return bad("That transaction uses " + tcur + " but the group uses " + cur + ".")
		}
		var oldID, oldGroup, oldName string
		oerr := tx.QueryRow(ctx, `SELECT i.id::text, i.group_id::text, g.name FROM group_incomes i JOIN groups g ON g.id=i.group_id
			WHERE i.transaction_id=$1 AND i.voided_at IS NULL FOR UPDATE OF i`, tid).Scan(&oldID, &oldGroup, &oldName)
		switch {
		case dbx.IsNoRows(oerr):
		case oerr != nil:
			return oerr
		case oldGroup == groupID:
			return apierr.New(http.StatusConflict, "already_shared", "This income is already in this group.")
		case !in.Move:
			return apierr.New(http.StatusConflict, "already_shared", "This income is already in \""+oldName+"\". Move it here to take it out of there.")
		default:
			if _, _, err := me(ctx, tx, userID, oldGroup); err != nil {
				return apierr.New(http.StatusConflict, "already_shared", "This income is in a group you are no longer part of, so it cannot be moved.")
			}
			if _, oldArchived, err := s.groupMeta(ctx, tx, oldGroup); err != nil {
				return err
			} else if oldArchived {
				return apierr.New(http.StatusConflict, "already_shared", "This income is in \""+oldName+"\", which is archived and read-only, so it cannot be moved.")
			}
			if _, err := tx.Exec(ctx, `UPDATE group_incomes SET voided_at=now() WHERE id=$1`, oldID); err != nil {
				return err
			}
		}
		out, err = scanIncome(tx.QueryRow(ctx, `INSERT INTO group_incomes (group_id, transaction_id, created_by, title, amount, currency, occurred_at)
			SELECT $1,$2,$3,$4,$5,$6,$7 RETURNING id::text, group_id::text, transaction_id::text, title, amount, currency, occurred_at, false`,
			groupID, tid, myID, title, amount, cur, when))
		return err
	})
	return out, err
}

// ListIncomes returns the caller's own incomes filed under the group: other members' incomes are their private ledger and never shown.
func (s *Service) ListIncomes(ctx context.Context, userID, groupID string) ([]GroupIncome, error) {
	if _, _, err := me(ctx, s.db, userID, groupID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT `+incomeCols+` FROM group_incomes i JOIN transactions t ON t.id=i.transaction_id AND t.user_id=$2
		WHERE i.group_id=$1 AND i.voided_at IS NULL AND t.deleted_at IS NULL AND t.status IN ('pending','confirmed')
		ORDER BY i.occurred_at DESC, i.id DESC LIMIT 200`, groupID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GroupIncome{}
	for rows.Next() {
		g, err := scanIncome(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// VoidIncome takes the income out of the group. Only the person whose income it is may do it; the transaction itself is untouched.
func (s *Service) VoidIncome(ctx context.Context, userID, id string) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE group_incomes i SET voided_at=now() FROM transactions t
			WHERE i.id=$1 AND i.voided_at IS NULL AND t.id=i.transaction_id AND t.user_id=$2`, id, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apierr.ErrNotFound
		}
		return nil
	})
}

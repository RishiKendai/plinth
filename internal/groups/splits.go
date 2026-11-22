package groups

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"plinth/server/internal/idempotency"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
)

type SplitPerson struct {
	ID    string `json:"id"` // chosen by the app so it can preview shares before saving; a UUID
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

// SplitInput shares one of the caller's own expenses with a few people who are not part of any group. The caller paid.
type SplitInput struct {
	ID            string        `json:"id"`
	TransactionID string        `json:"transaction_id"`
	Method        string        `json:"split_method"`
	MyID          string        `json:"my_id"`
	MyValue       int64         `json:"my_value"`
	People        []SplitPerson `json:"people"`
}

const maxSplitPeople = 20

// CreateSplit makes a group of kind 'split' named after the transaction, adds the people, and records the shared expense, all in
// one database transaction: either the whole split exists or nothing does. It uses the same engine as every group expense.
func (s *Service) CreateSplit(ctx context.Context, userID, key string, in SplitInput) (Group, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Group{}, err
	}
	f := map[string]string{}
	tid := ids.Normalize(in.TransactionID)
	if !ids.Valid(tid) {
		f["transaction_id"] = "Choose one of your transactions."
	}
	if len(in.People) == 0 || len(in.People) > maxSplitPeople {
		f["people"] = "Add between 1 and 20 people to split with."
	}
	gid := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(gid) {
		f["id"] = "Identifier must be a UUID."
	}
	if gid == "" {
		gid = ids.New()
	}
	myID := ids.Normalize(in.MyID)
	if in.MyID != "" && !ids.Valid(myID) {
		f["my_id"] = "Identifier must be a UUID."
	}
	if myID == "" {
		myID = ids.New()
	}
	type person struct {
		id, name string
		value    int64
	}
	var people []person
	seen := map[string]bool{myID: true}
	for _, p := range in.People {
		n, ok := cleanName(p.Name, 60)
		pid := ids.Normalize(p.ID)
		if p.ID != "" && !ids.Valid(pid) {
			f["people"] = "Each person needs a valid identifier."
		}
		if pid == "" {
			pid = ids.New()
		}
		if !ok || seen[pid] {
			f["people"] = "Each person needs a different name of 1 to 60 characters."
		}
		seen[pid] = true
		people = append(people, person{pid, n, p.Value})
	}
	if len(f) > 0 {
		return Group{}, apierr.Validation(f)
	}
	var out Group
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "split.create", idempotency.Hash("split.create", in))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = s.get(ctx, tx, userID, replay.ResourceID)
			return err
		}
		var title, cur string
		var amount int64
		var when time.Time
		if err := tx.QueryRow(ctx, `SELECT title, currency, amount, occurred_at FROM transactions WHERE id=$1 AND user_id=$2`, tid, userID).Scan(&title, &cur, &amount, &when); err != nil {
			if dbx.IsNoRows(err) {
				return apierr.Validation(map[string]string{"transaction_id": "Choose one of your transactions."})
			}
			return err
		}
		var display, email string
		if err := tx.QueryRow(ctx, `SELECT display_name, email FROM users WHERE id=$1`, userID).Scan(&display, &email); err != nil {
			return err
		}
		if strings.TrimSpace(display) == "" {
			display = strings.Split(email, "@")[0]
		}
		name, _ := cleanName(title, 80)
		if name == "" {
			name = "Split"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO groups (id, name, currency, created_by, kind) VALUES ($1,$2,$3,$4,'split')`, gid, name, cur, userID); err != nil {
			if dbx.ConstraintName(err) == "groups_pkey" {
				return apierr.New(http.StatusConflict, "id_taken", "That identifier is already used.")
			}
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO group_members (id, group_id, user_id, display_name, role) VALUES ($1,$2,$3,$4,'admin')`, myID, gid, userID, clip(display, 60)); err != nil {
			return err
		}
		parts := []ParticipantInput{{MemberID: myID, Value: in.MyValue}}
		for _, p := range people {
			if _, err := tx.Exec(ctx, `INSERT INTO group_members (id, group_id, display_name) VALUES ($1,$2,$3)`, p.id, gid, p.name); err != nil {
				return err
			}
			parts = append(parts, ParticipantInput{MemberID: p.id, Value: p.value})
		}
		occurred := when.UTC()
		exp := ExpenseInput{PaidBy: myID, Title: title, Amount: amount, SplitMethod: in.Method, Participants: parts, TransactionID: tid, OccurredAt: &occurred}
		if err := s.addExpenseInTx(ctx, tx, userID, gid, ids.New(), name, exp); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "group", gid); err != nil {
			return err
		}
		out, err = s.get(ctx, tx, userID, gid)
		return err
	})
	return out, err
}

// ShareInfo says whether one of the caller's transactions is shared, and where.
type ShareInfo struct {
	GroupID   string `json:"group_id"`
	GroupName string `json:"group_name"`
	Kind      string `json:"kind"`
	ExpenseID string `json:"expense_id"`
	MyShare   int64  `json:"my_share"`
	Total     int64  `json:"total"`
	// Participants is how many people the expense is split between (1 means it sits in the group whole).
	Participants int `json:"participants"`
}

// ShareOf returns the live share of a transaction the caller owns, or nil.
func (s *Service) ShareOf(ctx context.Context, userID, txID string) (*ShareInfo, error) {
	var si ShareInfo
	err := s.db.QueryRow(ctx, `SELECT g.id::text, g.name, g.kind, e.id::text, e.amount,
		COALESCE((SELECT sh.amount FROM expense_shares sh WHERE sh.expense_id=e.id AND sh.member_id=m.id),0),
		(SELECT count(*) FROM expense_shares sx WHERE sx.expense_id=e.id)
		FROM group_expenses e JOIN groups g ON g.id=e.group_id JOIN group_members m ON m.group_id=g.id AND m.user_id=$2 AND m.removed_at IS NULL
		JOIN transactions t ON t.id=e.transaction_id AND t.user_id=$2
		WHERE e.transaction_id=$1 AND e.voided_at IS NULL`, txID, userID).Scan(&si.GroupID, &si.GroupName, &si.Kind, &si.ExpenseID, &si.Total, &si.MyShare, &si.Participants)
	if dbx.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &si, nil
}

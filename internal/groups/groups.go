// Package groups owns shared expenses (CLAUDE.md 3.11, 3.12): groups, members, split expenses and settlements.
//
// The expense (what was paid), the obligations (each member's share) and settlements (actual repayments) are three
// separate records. A group is a separate entity: nothing here reads or writes another user's accounts or
// transactions. A member's personal ledger is touched only by that member's own request, through the transactions
// module, and a group admin has no route to it.
package groups

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/accounts"
	"plinth/server/internal/audit"
	"plinth/server/internal/idempotency"
	"plinth/server/internal/money"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
	"plinth/server/internal/settings"
	"plinth/server/internal/transactions"
)

var (
	ErrForbidden  = apierr.New(http.StatusForbidden, "forbidden", "Only a group admin can do that.")
	ErrLastAdmin  = apierr.New(http.StatusConflict, "last_admin", "A group needs at least one admin. Make someone else an admin first.")
	ErrOwes       = apierr.New(http.StatusConflict, "balance_not_zero", "That person has an unsettled balance. Settle up before removing them.")
	ErrArchived   = apierr.New(http.StatusConflict, "group_archived", "This group is archived.")
	ErrBadInvite  = apierr.New(http.StatusNotFound, "invite_invalid", "That invite code is not valid, or it has expired.")
	ErrAlreadyIn  = apierr.New(http.StatusConflict, "already_member", "You are already in this group.")
	ErrInviteSlot = apierr.New(http.StatusConflict, "not_invitable", "Only a person who is not on Plinth yet can be invited. Add them as a member first.")
)

const inviteTTL = 7 * 24 * time.Hour

type Group struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Currency    string     `json:"currency"`
	MyMemberID  string     `json:"my_member_id"`
	MyRole      string     `json:"my_role"`
	MemberCount int        `json:"member_count"`
	MyNet       int64      `json:"my_net"` // positive: the group owes you
	ArchivedAt  *time.Time `json:"archived_at"`
	Version     int        `json:"version"`
	Kind        string     `json:"kind"`         // group | split
	Purpose     string     `json:"purpose"`      // budget | tracker, fixed at creation
	BudgetLimit *int64     `json:"budget_limit"` // a budget's opening amount (null for a tracker)
	TopupTotal  int64      `json:"topup_total"`  // everything added to a budget since
	SpentTotal  int64      `json:"spent_total"`  // everything the group has spent
	Remaining   *int64     `json:"remaining"`    // budget: opening amount + top-ups - spent; null for a tracker
	MonthSpent  int64      `json:"month_spent"`  // what the group spent this (UTC) month
	CreatedAt   time.Time  `json:"created_at"`
}

type Member struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Role       string `json:"role"`
	OnPlinth   bool   `json:"on_plinth"`
	You        bool   `json:"you"`
	Removed    bool   `json:"removed"`
	Paid       int64  `json:"paid"`
	Owed       int64  `json:"owed"`
	SettledIn  int64  `json:"settled_in"`  // repayments received
	SettledOut int64  `json:"settled_out"` // repayments made
	Net        int64  `json:"net"`         // paid - owed + settled_out - settled_in; positive: the group owes them
}

type Balances struct {
	Members       []Member  `json:"members"`
	Payments      []Payment `json:"payments"` // the fewest repayments that settle everyone
	TotalExpenses int64     `json:"total_expenses"`
	TotalSettled  int64     `json:"total_settled"`
	Currency      string    `json:"currency"`
}

type Payment struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Amount int64  `json:"amount"`
}

type ShareView struct {
	MemberID string `json:"member_id"`
	Amount   int64  `json:"amount"`
	Input    int64  `json:"input"`
}

type Expense struct {
	ID            string      `json:"id"`
	GroupID       string      `json:"group_id"`
	PaidBy        string      `json:"paid_by"`
	CreatedBy     string      `json:"created_by"`
	Title         string      `json:"title"`
	Amount        int64       `json:"amount"`
	Currency      string      `json:"currency"`
	SplitMethod   string      `json:"split_method"`
	OccurredAt    time.Time   `json:"occurred_at"`
	Notes         string      `json:"notes"`
	Voided        bool        `json:"voided"`
	Shares        []ShareView `json:"shares"`
	InMyLedger    bool        `json:"in_my_ledger"` // true only for the member who recorded their own payment
	CreatedAt     time.Time   `json:"created_at"`
	transactionID *string
	ownerUserID   *string
}

type Settlement struct {
	ID         string    `json:"id"`
	GroupID    string    `json:"group_id"`
	From       string    `json:"from_member"`
	To         string    `json:"to_member"`
	Amount     int64     `json:"amount"`
	Currency   string    `json:"currency"`
	OccurredAt time.Time `json:"occurred_at"`
	Note       string    `json:"note"`
	Voided     bool      `json:"voided"`
	InMyLedger bool      `json:"in_my_ledger"`
	CreatedAt  time.Time `json:"created_at"`
}

type Service struct {
	db       *pgxpool.Pool
	settings *settings.Service
	txs      *transactions.Service
	now      func() time.Time
}

func NewService(db *pgxpool.Pool, st *settings.Service, txs *transactions.Service) *Service {
	return &Service{db: db, settings: st, txs: txs, now: time.Now}
}

func cleanName(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	return s, n >= 1 && n <= max
}

// me returns the caller's member row in a group. A non-member gets 404, exactly like a group that does not exist,
// so group ids cannot be probed.
func me(ctx context.Context, q dbx.Q, userID, groupID string) (memberID, role string, err error) {
	err = q.QueryRow(ctx, `SELECT id::text, role FROM group_members WHERE group_id=$1 AND user_id=$2 AND removed_at IS NULL`, groupID, userID).Scan(&memberID, &role)
	if dbx.IsNoRows(err) {
		return "", "", apierr.ErrNotFound
	}
	return
}

func admin(ctx context.Context, q dbx.Q, userID, groupID string) (string, error) {
	m, role, err := me(ctx, q, userID, groupID)
	if err != nil {
		return "", err
	}
	if role != "admin" {
		return "", ErrForbidden
	}
	return m, nil
}

func (s *Service) groupMeta(ctx context.Context, q dbx.Q, groupID string) (currency string, archived bool, err error) {
	var at *time.Time
	err = q.QueryRow(ctx, `SELECT currency, archived_at FROM groups WHERE id=$1`, groupID).Scan(&currency, &at)
	return currency, at != nil, err
}

// ---- groups ----

type CreateInput struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
	// Purpose is chosen once and never changes: "budget" (needs a monthly BudgetLimit) or "tracker" (no limit). Empty means tracker.
	Purpose     string `json:"purpose"`
	BudgetLimit *int64 `json:"budget_limit"`
}

func (s *Service) Create(ctx context.Context, userID, key string, in CreateInput) (Group, error) {
	if err := idempotency.Check(key, false); err != nil {
		return Group{}, err
	}
	f := map[string]string{}
	name, ok := cleanName(in.Name, 80)
	if !ok {
		f["name"] = "Enter a name of 1 to 80 characters."
	}
	st, err := s.settings.Get(ctx, userID)
	if err != nil {
		return Group{}, err
	}
	if in.Currency == "" {
		in.Currency = st.Currency
	}
	if _, ok := money.Lookup(in.Currency); !ok {
		f["currency"] = "Choose a supported currency."
	}
	purpose := in.Purpose
	if purpose == "" {
		purpose = "tracker"
	}
	switch {
	case purpose != "budget" && purpose != "tracker":
		f["purpose"] = "Choose budget or spending tracker."
	case purpose == "budget" && (in.BudgetLimit == nil || *in.BudgetLimit <= 0 || *in.BudgetLimit > 1_000_000_000_000_000):
		f["budget_limit"] = "A budget needs an amount greater than zero to start with."
	case purpose == "tracker" && in.BudgetLimit != nil:
		f["budget_limit"] = "A spending tracker has no money to set aside."
	}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		f["id"] = "Identifier must be a UUID."
	}
	if id == "" {
		id = ids.New()
	}
	if len(f) > 0 {
		return Group{}, apierr.Validation(f)
	}
	var out Group
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "group.create", idempotency.Hash("group.create", in))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = s.get(ctx, tx, userID, replay.ResourceID)
			return err
		}
		var display, email string
		if err := tx.QueryRow(ctx, `SELECT display_name, email FROM users WHERE id=$1`, userID).Scan(&display, &email); err != nil {
			return err
		}
		if strings.TrimSpace(display) == "" {
			display = strings.Split(email, "@")[0]
		}
		if _, err := tx.Exec(ctx, `INSERT INTO groups (id, name, currency, created_by, purpose, budget_limit) VALUES ($1,$2,$3,$4,$5,$6)`, id, name, in.Currency, userID, purpose, in.BudgetLimit); err != nil {
			if dbx.ConstraintName(err) == "groups_pkey" {
				return transactions.ErrIDTaken
			}
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO group_members (id, group_id, user_id, display_name, role) VALUES ($1,$2,$3,$4,'admin')`, ids.New(), id, userID, clip(display, 60)); err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "group.created", map[string]any{"group_id": id}); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "group", id); err != nil {
			return err
		}
		out, err = s.get(ctx, tx, userID, id)
		return err
	})
	return out, err
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func (s *Service) get(ctx context.Context, q dbx.Q, userID, id string) (Group, error) {
	var g Group
	err := q.QueryRow(ctx, `SELECT g.id::text, g.name, g.currency, m.id::text, m.role, (SELECT count(*) FROM group_members x WHERE x.group_id=g.id AND x.removed_at IS NULL),
		g.archived_at, g.version, g.created_at, g.kind, g.budget_limit, g.purpose,
		COALESCE((SELECT SUM(t.amount) FROM group_topups t WHERE t.group_id=g.id AND t.voided_at IS NULL),0)::bigint,
		COALESCE((SELECT SUM(e.amount) FROM group_expenses e WHERE e.group_id=g.id AND e.voided_at IS NULL),0)::bigint,
		COALESCE((SELECT SUM(e.amount) FROM group_expenses e WHERE e.group_id=g.id AND e.voided_at IS NULL AND e.occurred_at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'),0)::bigint FROM groups g JOIN group_members m ON m.group_id=g.id AND m.user_id=$2 AND m.removed_at IS NULL WHERE g.id=$1`, id, userID).
		Scan(&g.ID, &g.Name, &g.Currency, &g.MyMemberID, &g.MyRole, &g.MemberCount, &g.ArchivedAt, &g.Version, &g.CreatedAt, &g.Kind, &g.BudgetLimit, &g.Purpose, &g.TopupTotal, &g.SpentTotal, &g.MonthSpent)
	if dbx.IsNoRows(err) {
		return g, apierr.ErrNotFound
	}
	g.CreatedAt = g.CreatedAt.UTC()
	if g.BudgetLimit != nil {
		rem := *g.BudgetLimit + g.TopupTotal - g.SpentTotal
		g.Remaining = &rem
	}
	if err == nil {
		b, berr := s.balances(ctx, q, userID, id)
		if berr == nil {
			for _, m := range b.Members {
				if m.You {
					g.MyNet = m.Net
				}
			}
		}
	}
	return g, err
}

func (s *Service) Get(ctx context.Context, userID, id string) (Group, error) {
	return s.get(ctx, s.db, userID, id)
}

func (s *Service) List(ctx context.Context, userID string) ([]Group, error) {
	return s.list(ctx, userID, "group")
}

// ListSplits returns the caller's splits (see CreateSplit), newest first.
func (s *Service) ListSplits(ctx context.Context, userID string) ([]Group, error) {
	return s.list(ctx, userID, "split")
}

func (s *Service) list(ctx context.Context, userID, kind string) ([]Group, error) {
	rows, err := s.db.Query(ctx, `SELECT g.id::text FROM groups g JOIN group_members m ON m.group_id=g.id AND m.user_id=$1 AND m.removed_at IS NULL
		WHERE g.kind = '`+kind+`' ORDER BY g.archived_at IS NOT NULL, GREATEST(g.created_at, COALESCE((SELECT max(e.created_at) FROM group_expenses e WHERE e.group_id=g.id), g.created_at)) DESC, g.id`, userID)
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
	out := []Group{}
	for _, id := range idList {
		g, err := s.get(ctx, s.db, userID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *Service) Rename(ctx context.Context, userID, id, name string, version int) (Group, error) {
	if _, err := admin(ctx, s.db, userID, id); err != nil {
		return Group{}, err
	}
	n, ok := cleanName(name, 80)
	if !ok {
		return Group{}, apierr.Validation(map[string]string{"name": "Enter a name of 1 to 80 characters."})
	}
	tag, err := s.db.Exec(ctx, `UPDATE groups SET name=$2, version=version+1, updated_at=now() WHERE id=$1 AND version=$3`, id, n, version)
	if err != nil {
		return Group{}, err
	}
	if tag.RowsAffected() == 0 {
		return Group{}, apierr.New(http.StatusConflict, "version_conflict", "This group changed since you loaded it. Reload and try again.")
	}
	return s.get(ctx, s.db, userID, id)
}

func (s *Service) SetArchived(ctx context.Context, userID, id string, archive bool) (Group, error) {
	if _, err := admin(ctx, s.db, userID, id); err != nil {
		return Group{}, err
	}
	if _, err := s.db.Exec(ctx, `UPDATE groups SET archived_at = CASE WHEN $2 THEN COALESCE(archived_at, now()) ELSE NULL END, version=version+1, updated_at=now() WHERE id=$1`, id, archive); err != nil {
		return Group{}, err
	}
	_ = audit.Log(ctx, s.db, userID, map[bool]string{true: "group.archived", false: "group.unarchived"}[archive], map[string]any{"group_id": id})
	return s.get(ctx, s.db, userID, id)
}

// ---- members ----

func (s *Service) AddMember(ctx context.Context, userID, groupID, name string) (Member, error) {
	if _, err := admin(ctx, s.db, userID, groupID); err != nil {
		return Member{}, err
	}
	n, ok := cleanName(name, 60)
	if !ok {
		return Member{}, apierr.Validation(map[string]string{"name": "Enter a name of 1 to 60 characters."})
	}
	id := ids.New()
	if _, err := s.db.Exec(ctx, `INSERT INTO group_members (id, group_id, display_name) VALUES ($1,$2,$3)`, id, groupID, n); err != nil {
		return Member{}, err
	}
	_ = audit.Log(ctx, s.db, userID, "group.member_added", map[string]any{"group_id": groupID, "member_id": id})
	return Member{ID: id, Name: n, Role: "member"}, nil
}

// RemoveMember removes a person from the group (soft). An admin can remove anyone; anyone can leave. A person with an
// unsettled balance cannot be removed, and the last admin cannot leave.
func (s *Service) RemoveMember(ctx context.Context, userID, groupID, memberID string) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		myID, role, err := me(ctx, tx, userID, groupID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM groups WHERE id=$1 FOR UPDATE`, groupID); err != nil { // serialise membership changes
			return err
		}
		var target string
		var trole string
		err = tx.QueryRow(ctx, `SELECT id::text, role FROM group_members WHERE id=$1 AND group_id=$2 AND removed_at IS NULL`, memberID, groupID).Scan(&target, &trole)
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if role != "admin" && target != myID {
			return ErrForbidden
		}
		if trole == "admin" {
			var admins int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM group_members WHERE group_id=$1 AND role='admin' AND removed_at IS NULL`, groupID).Scan(&admins); err != nil {
				return err
			}
			if admins <= 1 {
				return ErrLastAdmin
			}
		}
		b, err := s.balances(ctx, tx, userID, groupID)
		if err != nil {
			return err
		}
		for _, m := range b.Members {
			if m.ID == target && m.Net != 0 {
				return ErrOwes
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE group_members SET removed_at=now() WHERE id=$1`, target); err != nil {
			return err
		}
		return audit.Log(ctx, tx, userID, "group.member_removed", map[string]any{"group_id": groupID, "member_id": target})
	})
}

func (s *Service) SetRole(ctx context.Context, userID, groupID, memberID, role string) error {
	if role != "admin" && role != "member" {
		return apierr.Validation(map[string]string{"role": "Choose admin or member."})
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := admin(ctx, tx, userID, groupID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM groups WHERE id=$1 FOR UPDATE`, groupID); err != nil {
			return err
		}
		var cur string
		err := tx.QueryRow(ctx, `SELECT role FROM group_members WHERE id=$1 AND group_id=$2 AND removed_at IS NULL`, memberID, groupID).Scan(&cur)
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur == "admin" && role == "member" {
			var admins int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM group_members WHERE group_id=$1 AND role='admin' AND removed_at IS NULL`, groupID).Scan(&admins); err != nil {
				return err
			}
			if admins <= 1 {
				return ErrLastAdmin
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE group_members SET role=$2 WHERE id=$1`, memberID, role); err != nil {
			return err
		}
		return audit.Log(ctx, tx, userID, "group.role_changed", map[string]any{"group_id": groupID, "member_id": memberID, "role": role})
	})
}

// Invite creates a one-time code that lets a person who uses Plinth take over a member slot. The code is shown once;
// only its hash is stored.
func (s *Service) Invite(ctx context.Context, userID, groupID, memberID string) (string, time.Time, error) {
	if _, err := admin(ctx, s.db, userID, groupID); err != nil {
		return "", time.Time{}, err
	}
	var uid *string
	err := s.db.QueryRow(ctx, `SELECT user_id::text FROM group_members WHERE id=$1 AND group_id=$2 AND removed_at IS NULL`, memberID, groupID).Scan(&uid)
	if dbx.IsNoRows(err) {
		return "", time.Time{}, apierr.ErrNotFound
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if uid != nil {
		return "", time.Time{}, ErrInviteSlot
	}
	b := make([]byte, 7)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	code := strings.ToUpper(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:10]
	h := sha256.Sum256([]byte(code))
	exp := s.now().Add(inviteTTL).UTC()
	if _, err := s.db.Exec(ctx, `UPDATE group_invites SET used_at=now() WHERE member_id=$1 AND used_at IS NULL`, memberID); err != nil { // one live code per slot
		return "", time.Time{}, err
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO group_invites (id, group_id, member_id, code_hash, created_by, expires_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		ids.New(), groupID, memberID, h[:], userID, exp); err != nil {
		return "", time.Time{}, err
	}
	_ = audit.Log(ctx, s.db, userID, "group.invited", map[string]any{"group_id": groupID, "member_id": memberID})
	return code, exp, nil
}

// Redeem binds the caller to the invited member slot.
func (s *Service) Redeem(ctx context.Context, userID, code string) (Group, error) {
	code = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), " ", ""))
	h := sha256.Sum256([]byte(code))
	var gid string
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var inviteID, memberID string
		var exp time.Time
		err := tx.QueryRow(ctx, `SELECT id::text, group_id::text, member_id::text, expires_at FROM group_invites WHERE code_hash=$1 AND used_at IS NULL FOR UPDATE`, h[:]).Scan(&inviteID, &gid, &memberID, &exp)
		if dbx.IsNoRows(err) || (err == nil && !exp.After(s.now())) {
			return ErrBadInvite
		}
		if err != nil {
			return err
		}
		var in int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM group_members WHERE group_id=$1 AND user_id=$2 AND removed_at IS NULL`, gid, userID).Scan(&in); err != nil {
			return err
		}
		if in > 0 {
			return ErrAlreadyIn
		}
		var display, email string
		if err := tx.QueryRow(ctx, `SELECT display_name, email FROM users WHERE id=$1`, userID).Scan(&display, &email); err != nil {
			return err
		}
		if strings.TrimSpace(display) == "" {
			display = strings.Split(email, "@")[0]
		}
		tag, err := tx.Exec(ctx, `UPDATE group_members SET user_id=$2 WHERE id=$1 AND user_id IS NULL AND removed_at IS NULL`, memberID, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrBadInvite
		}
		if _, err := tx.Exec(ctx, `UPDATE group_invites SET used_at=now() WHERE id=$1`, inviteID); err != nil {
			return err
		}
		_ = display // the slot keeps the name the admin gave it
		return audit.Log(ctx, tx, userID, "group.joined", map[string]any{"group_id": gid, "member_id": memberID})
	})
	if err != nil {
		return Group{}, err
	}
	return s.get(ctx, s.db, userID, gid)
}

// ---- balances ----

func (s *Service) balances(ctx context.Context, q dbx.Q, userID, groupID string) (Balances, error) {
	if _, _, err := me(ctx, q, userID, groupID); err != nil {
		return Balances{}, err
	}
	cur, _, err := s.groupMeta(ctx, q, groupID)
	if err != nil {
		return Balances{}, err
	}
	rows, err := q.Query(ctx, `SELECT gm.id::text, gm.display_name, gm.role, gm.user_id IS NOT NULL, gm.user_id = $2, gm.removed_at IS NOT NULL,
		COALESCE((SELECT SUM(amount) FROM group_expenses WHERE group_id=$1 AND paid_by=gm.id AND voided_at IS NULL),0),
		COALESCE((SELECT SUM(s.amount) FROM expense_shares s JOIN group_expenses e ON e.id=s.expense_id WHERE e.group_id=$1 AND s.member_id=gm.id AND e.voided_at IS NULL),0),
		COALESCE((SELECT SUM(amount) FROM group_settlements WHERE group_id=$1 AND from_member=gm.id AND voided_at IS NULL),0),
		COALESCE((SELECT SUM(amount) FROM group_settlements WHERE group_id=$1 AND to_member=gm.id AND voided_at IS NULL),0)
		FROM group_members gm WHERE gm.group_id=$1 ORDER BY gm.removed_at IS NOT NULL, gm.created_at, gm.id`, groupID, userID)
	if err != nil {
		return Balances{}, err
	}
	defer rows.Close()
	b := Balances{Currency: cur, Members: []Member{}, Payments: []Payment{}}
	net := map[string]int64{}
	for rows.Next() {
		var m Member
		var you *bool
		if err := rows.Scan(&m.ID, &m.Name, &m.Role, &m.OnPlinth, &you, &m.Removed, &m.Paid, &m.Owed, &m.SettledOut, &m.SettledIn); err != nil {
			return b, err
		}
		m.You = you != nil && *you
		m.Net = m.Paid - m.Owed + m.SettledOut - m.SettledIn
		net[m.ID] = m.Net
		b.TotalExpenses += m.Paid
		b.TotalSettled += m.SettledOut
		b.Members = append(b.Members, m)
	}
	if err := rows.Err(); err != nil {
		return b, err
	}
	pays, err := Simplify(net)
	if err != nil {
		return b, err
	}
	for _, p := range pays {
		b.Payments = append(b.Payments, Payment{From: p.From, To: p.To, Amount: p.Amount})
	}
	sort.SliceStable(b.Members, func(i, j int) bool { return !b.Members[i].Removed && b.Members[j].Removed })
	return b, nil
}

func (s *Service) Balances(ctx context.Context, userID, groupID string) (Balances, error) {
	return s.balances(ctx, s.db, userID, groupID)
}

// ---- expenses ----

type ParticipantInput struct {
	MemberID string `json:"member_id"`
	Value    int64  `json:"value"` // exact amount, basis points (percent x 100) or share weight; ignored for equal
}

type ExpenseInput struct {
	ID           string             `json:"id"`
	PaidBy       string             `json:"paid_by"`
	Title        string             `json:"title"`
	Amount       int64              `json:"amount"`
	SplitMethod  string             `json:"split_method"`
	Participants []ParticipantInput `json:"participants"`
	OccurredAt   *time.Time         `json:"occurred_at"`
	Notes        string             `json:"notes"`
	// AccountID records the payment in the caller's own ledger. Allowed only when the caller is the payer.
	AccountID  string  `json:"account_id"`
	CategoryID *string `json:"category_id"`
	// TransactionID shares an existing personal expense of the caller instead of creating one: its amount must equal the expense, the
	// caller must be the payer, and from then on only the caller's own share counts as spending (counted_amount).
	TransactionID string `json:"transaction_id"`
}

// ComputeShares validates participants against a group and runs the split engine.
func (s *Service) activeMembers(ctx context.Context, q dbx.Q, groupID string) (map[string]bool, error) {
	rows, err := q.Query(ctx, `SELECT id::text FROM group_members WHERE group_id=$1 AND removed_at IS NULL`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Service) AddExpense(ctx context.Context, userID, key, groupID string, in ExpenseInput) (Expense, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Expense{}, err
	}
	f := map[string]string{}
	title, ok := cleanName(in.Title, 120)
	if !ok {
		f["title"] = "Enter a title of 1 to 120 characters."
	}
	if in.Amount <= 0 || in.Amount > 1_000_000_000_000_000 {
		f["amount"] = "Enter an amount greater than zero."
	}
	if utf8.RuneCountInString(in.Notes) > 1000 {
		f["notes"] = "Use 1,000 characters or fewer."
	}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		f["id"] = "Identifier must be a UUID."
	}
	if id == "" {
		id = ids.New()
	}
	if len(f) > 0 {
		return Expense{}, apierr.Validation(f)
	}
	var out Expense
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "group.expense", idempotency.Hash("group.expense", []any{groupID, in}))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = s.expense(ctx, tx, userID, replay.ResourceID)
			return err
		}
		if err := s.addExpenseInTx(ctx, tx, userID, groupID, id, title, in); err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "group.expense_added", map[string]any{"group_id": groupID, "expense_id": id}); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "group_expense", id); err != nil {
			return err
		}
		out, err = s.expense(ctx, tx, userID, id)
		return err
	})
	return out, err
}

// addExpenseInTx records one expense (and its shares) inside the caller's transaction. It is shared by AddExpense and CreateSplit so
// both follow exactly the same split, ledger and linking rules.
func (s *Service) addExpenseInTx(ctx context.Context, tx pgx.Tx, userID, groupID, id, title string, in ExpenseInput) error {
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
	// A budget is a pot: lock it, and refuse an expense bigger than what is left (reaching exactly zero is fine).
	var purpose string
	var remaining int64
	if err := tx.QueryRow(ctx, `SELECT g.purpose, COALESCE(g.budget_limit,0)
		+ COALESCE((SELECT SUM(t.amount) FROM group_topups t WHERE t.group_id=g.id AND t.voided_at IS NULL),0)
		- COALESCE((SELECT SUM(e.amount) FROM group_expenses e WHERE e.group_id=g.id AND e.voided_at IS NULL),0)
		FROM groups g WHERE g.id=$1 FOR UPDATE`, groupID).Scan(&purpose, &remaining); err != nil {
		return err
	}
	if purpose == "budget" && in.Amount > remaining {
		return apierr.New(http.StatusConflict, "budget_exhausted", "This budget does not have enough left for that expense. Top it up first.")
	}
	active, err := s.activeMembers(ctx, tx, groupID)
	if err != nil {
		return err
	}
	paidBy := ids.Normalize(in.PaidBy)
	if paidBy == "" {
		paidBy = myID
	}
	if !active[paidBy] {
		return apierr.Validation(map[string]string{"paid_by": "Choose someone who is in the group."})
	}
	parts := make([]Part, 0, len(in.Participants))
	for _, p := range in.Participants {
		mid := ids.Normalize(p.MemberID)
		if !active[mid] {
			return apierr.Validation(map[string]string{"participants": "Everyone you split with must be in the group."})
		}
		parts = append(parts, Part{Member: mid, Value: p.Value})
	}
	shares, err := Compute(in.SplitMethod, in.Amount, parts)
	if err != nil {
		return apierr.Validation(map[string]string{"participants": capital(err.Error()) + "."})
	}
	when := s.now().UTC()
	if in.OccurredAt != nil {
		when = in.OccurredAt.UTC()
	}
	var txID *string
	if in.AccountID != "" {
		if paidBy != myID {
			return apierr.Validation(map[string]string{"account_id": "You can record a payment in your own accounts only when you are the one who paid."})
		}
		aid := ids.Normalize(in.AccountID)
		acct, err := accounts.Fetch(ctx, tx, userID, aid)
		if err != nil || acct.ArchivedAt != nil {
			return apierr.Validation(map[string]string{"account_id": "Choose one of your active accounts."})
		}
		if acct.Currency != cur {
			return apierr.Validation(map[string]string{"account_id": "That account uses " + acct.Currency + " but the group uses " + cur + "."})
		}
		var mine int64 // what counts as the caller's own spending: only their share, not what they fronted for others
		for _, sh := range shares {
			if sh.Member == myID {
				mine = sh.Amount
			}
		}
		t, err := s.txs.CreateInTx(ctx, tx, userID, transactions.NewTx{AccountID: aid, Type: "expense", Amount: in.Amount, Title: title, CategoryID: in.CategoryID,
			Status: "confirmed", OccurredAt: when, Timezone: "Asia/Kolkata", Source: "group", ExternalRef: "group:" + id, CountedAmount: &mine, Notes: "Group expense"})
		if err != nil {
			return err
		}
		txID = &t
	}
	if in.TransactionID != "" {
		if in.AccountID != "" {
			return apierr.Validation(map[string]string{"account_id": "Choose either an existing transaction or an account, not both."})
		}
		if paidBy != myID {
			return apierr.Validation(map[string]string{"transaction_id": "You can share your own payment only when you are the one who paid."})
		}
		tid := ids.Normalize(in.TransactionID)
		bad := func(m string) error { return apierr.Validation(map[string]string{"transaction_id": m}) }
		if !ids.Valid(tid) {
			return bad("Choose one of your transactions.")
		}
		var typ, tcur, tstatus, tsource string
		var tamount int64
		err := tx.QueryRow(ctx, `SELECT type, currency, status, source, amount FROM transactions WHERE id=$1 AND user_id=$2 FOR UPDATE`, tid, userID).Scan(&typ, &tcur, &tstatus, &tsource, &tamount)
		switch {
		case dbx.IsNoRows(err):
			return bad("Choose one of your transactions.")
		case err != nil:
			return err
		case typ != "expense" || (tstatus != "pending" && tstatus != "confirmed"):
			return bad("Only a recorded expense can be shared.")
		case tsource == "group" || tsource == "settlement":
			return bad("That transaction already belongs to a group.")
		case tcur != cur:
			return bad("That transaction uses " + tcur + " but the group uses " + cur + ".")
		case tamount != in.Amount:
			return bad("The shared amount must equal the transaction's amount.")
		}
		var shared bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM group_expenses WHERE transaction_id=$1 AND voided_at IS NULL)`, tid).Scan(&shared); err != nil {
			return err
		}
		if shared {
			return apierr.New(http.StatusConflict, "already_shared", "This transaction is already shared. Void the share first to share it somewhere else.")
		}
		var mine int64
		for _, sh := range shares {
			if sh.Member == myID {
				mine = sh.Amount
			}
		}
		// Only counted_amount changes: the balance and the ledger are untouched. Analytics, budgets and trackers now count the caller's share.
		if _, err := tx.Exec(ctx, `UPDATE transactions SET counted_amount=$2, version=version+1, updated_at=now() WHERE id=$1`, tid, mine); err != nil {
			return err
		}
		txID = &tid
	}
	if _, err := tx.Exec(ctx, `INSERT INTO group_expenses (id, group_id, paid_by, created_by, title, amount, currency, split_method, occurred_at, notes, transaction_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, groupID, paidBy, myID, title, in.Amount, cur, in.SplitMethod, when, in.Notes, txID); err != nil {
		if dbx.ConstraintName(err) == "group_expenses_pkey" {
			return transactions.ErrIDTaken
		}
		return err
	}
	for _, sh := range shares {
		if _, err := tx.Exec(ctx, `INSERT INTO expense_shares (expense_id, member_id, amount, input) VALUES ($1,$2,$3,$4)`, id, sh.Member, sh.Amount, sh.Input); err != nil {
			return err
		}
	}
	return nil
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (s *Service) expense(ctx context.Context, q dbx.Q, userID, id string) (Expense, error) {
	var e Expense
	var voided *time.Time
	var ownerTx *string
	err := q.QueryRow(ctx, `SELECT e.id::text, e.group_id::text, e.paid_by::text, e.created_by::text, e.title, e.amount, e.currency, e.split_method, e.occurred_at, e.notes, e.voided_at, e.created_at,
		e.transaction_id::text FROM group_expenses e JOIN group_members m ON m.group_id=e.group_id AND m.user_id=$2 AND m.removed_at IS NULL WHERE e.id=$1`, id, userID).
		Scan(&e.ID, &e.GroupID, &e.PaidBy, &e.CreatedBy, &e.Title, &e.Amount, &e.Currency, &e.SplitMethod, &e.OccurredAt, &e.Notes, &voided, &e.CreatedAt, &ownerTx)
	if dbx.IsNoRows(err) {
		return e, apierr.ErrNotFound
	}
	if err != nil {
		return e, err
	}
	e.OccurredAt, e.CreatedAt, e.Voided = e.OccurredAt.UTC(), e.CreatedAt.UTC(), voided != nil
	e.transactionID = ownerTx
	e.Shares = []ShareView{}
	rows, err := q.Query(ctx, `SELECT member_id::text, amount, input FROM expense_shares WHERE expense_id=$1 ORDER BY member_id`, id)
	if err != nil {
		return e, err
	}
	defer rows.Close()
	for rows.Next() {
		var sv ShareView
		if err := rows.Scan(&sv.MemberID, &sv.Amount, &sv.Input); err != nil {
			return e, err
		}
		e.Shares = append(e.Shares, sv)
	}
	if ownerTx != nil {
		var own string
		if err := q.QueryRow(ctx, `SELECT user_id::text FROM transactions WHERE id=$1`, *ownerTx).Scan(&own); err == nil {
			e.ownerUserID = &own
			e.InMyLedger = own == userID
		}
	}
	return e, rows.Err()
}

func (s *Service) GetExpense(ctx context.Context, userID, id string) (Expense, error) {
	return s.expense(ctx, s.db, userID, id)
}

func (s *Service) ListExpenses(ctx context.Context, userID, groupID string) ([]Expense, error) {
	if _, _, err := me(ctx, s.db, userID, groupID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT id::text FROM group_expenses WHERE group_id=$1 ORDER BY occurred_at DESC, id DESC LIMIT 200`, groupID)
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
	out := []Expense{}
	for _, id := range idList {
		e, err := s.expense(ctx, s.db, userID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// VoidExpense voids the group record. The creator or an admin may do it. The payer's own ledger entry, if there is
// one, is voided only when the payer is the one asking: nobody else, including an admin, can touch a personal ledger.
func (s *Service) VoidExpense(ctx context.Context, userID, id string) (Expense, error) {
	var out Expense
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		e, err := s.expense(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		myID, role, err := me(ctx, tx, userID, e.GroupID)
		if err != nil {
			return err
		}
		if e.CreatedBy != myID && e.PaidBy != myID && role != "admin" {
			return ErrForbidden
		}
		if !e.Voided {
			if _, err := tx.Exec(ctx, `UPDATE group_expenses SET voided_at=now() WHERE id=$1 AND voided_at IS NULL`, id); err != nil {
				return err
			}
			if e.InMyLedger && e.transactionID != nil {
				var src string
				if err := tx.QueryRow(ctx, `SELECT source FROM transactions WHERE id=$1 AND user_id=$2`, *e.transactionID, userID).Scan(&src); err != nil {
					return err
				}
				if src == "group" {
					if err := s.txs.VoidInTx(ctx, tx, userID, *e.transactionID); err != nil {
						return err
					}
				} else if _, err := tx.Exec(ctx, `UPDATE transactions SET counted_amount=NULL, version=version+1, updated_at=now() WHERE id=$1 AND user_id=$2`, *e.transactionID, userID); err != nil {
					return err // an existing transaction the caller shared goes back to counting in full; it is never voided by un-sharing
				}
			}
			if err := audit.Log(ctx, tx, userID, "group.expense_voided", map[string]any{"group_id": e.GroupID, "expense_id": id}); err != nil {
				return err
			}
		}
		out, err = s.expense(ctx, tx, userID, id)
		return err
	})
	return out, err
}

// ---- settlements ----

type SettlementInput struct {
	ID         string     `json:"id"`
	From       string     `json:"from_member"`
	To         string     `json:"to_member"`
	Amount     int64      `json:"amount"`
	OccurredAt *time.Time `json:"occurred_at"`
	Note       string     `json:"note"`
	// AccountID optionally records the repayment in the caller's own ledger. Allowed only when the caller is the payer
	// (an expense) or the receiver (income). It is excluded from income and expense analytics (source = settlement).
	AccountID string `json:"account_id"`
}

func (s *Service) AddSettlement(ctx context.Context, userID, key, groupID string, in SettlementInput) (Settlement, error) {
	if err := idempotency.Check(key, true); err != nil {
		return Settlement{}, err
	}
	f := map[string]string{}
	if in.Amount <= 0 || in.Amount > 1_000_000_000_000_000 {
		f["amount"] = "Enter an amount greater than zero."
	}
	if utf8.RuneCountInString(in.Note) > 500 {
		f["note"] = "Use 500 characters or fewer."
	}
	from, to := ids.Normalize(in.From), ids.Normalize(in.To)
	if from == to {
		f["to_member"] = "Choose someone other than the person paying."
	}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		f["id"] = "Identifier must be a UUID."
	}
	if id == "" {
		id = ids.New()
	}
	if len(f) > 0 {
		return Settlement{}, apierr.Validation(f)
	}
	var out Settlement
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "group.settlement", idempotency.Hash("group.settlement", []any{groupID, in}))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = s.settlement(ctx, tx, userID, replay.ResourceID)
			return err
		}
		myID, role, err := me(ctx, tx, userID, groupID)
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
		active, err := s.activeMembers(ctx, tx, groupID)
		if err != nil {
			return err
		}
		if !active[from] || !active[to] {
			return apierr.Validation(map[string]string{"from_member": "Both people must be in the group."})
		}
		if role != "admin" && from != myID && to != myID {
			return ErrForbidden // a member records only repayments they are part of
		}
		when := s.now().UTC()
		if in.OccurredAt != nil {
			when = in.OccurredAt.UTC()
		}
		var txID *string
		if in.AccountID != "" {
			if from != myID && to != myID {
				return apierr.Validation(map[string]string{"account_id": "You can record a repayment in your own accounts only when you paid or received it."})
			}
			aid := ids.Normalize(in.AccountID)
			acct, err := accounts.Fetch(ctx, tx, userID, aid)
			if err != nil || acct.ArchivedAt != nil {
				return apierr.Validation(map[string]string{"account_id": "Choose one of your active accounts."})
			}
			if acct.Currency != cur {
				return apierr.Validation(map[string]string{"account_id": "That account uses " + acct.Currency + " but the group uses " + cur + "."})
			}
			typ, title := "income", "Repayment received"
			if from == myID {
				typ, title = "expense", "Repayment to group member"
			}
			t, err := s.txs.CreateInTx(ctx, tx, userID, transactions.NewTx{AccountID: aid, Type: typ, Amount: in.Amount, Title: title, Status: "confirmed",
				OccurredAt: when, Timezone: "Asia/Kolkata", Source: "settlement", ExternalRef: "settlement:" + id, Notes: in.Note})
			if err != nil {
				return err
			}
			txID = &t
		}
		if _, err := tx.Exec(ctx, `INSERT INTO group_settlements (id, group_id, from_member, to_member, created_by, amount, currency, occurred_at, note, transaction_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, groupID, from, to, myID, in.Amount, cur, when, in.Note, txID); err != nil {
			if dbx.ConstraintName(err) == "group_settlements_pkey" {
				return transactions.ErrIDTaken
			}
			return err
		}
		if err := audit.Log(ctx, tx, userID, "group.settlement_added", map[string]any{"group_id": groupID, "settlement_id": id}); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "group_settlement", id); err != nil {
			return err
		}
		out, err = s.settlement(ctx, tx, userID, id)
		return err
	})
	return out, err
}

func (s *Service) settlement(ctx context.Context, q dbx.Q, userID, id string) (Settlement, error) {
	var st Settlement
	var voided *time.Time
	var txID *string
	err := q.QueryRow(ctx, `SELECT x.id::text, x.group_id::text, x.from_member::text, x.to_member::text, x.amount, x.currency, x.occurred_at, x.note, x.voided_at, x.created_at, x.transaction_id::text
		FROM group_settlements x JOIN group_members m ON m.group_id=x.group_id AND m.user_id=$2 AND m.removed_at IS NULL WHERE x.id=$1`, id, userID).
		Scan(&st.ID, &st.GroupID, &st.From, &st.To, &st.Amount, &st.Currency, &st.OccurredAt, &st.Note, &voided, &st.CreatedAt, &txID)
	if dbx.IsNoRows(err) {
		return st, apierr.ErrNotFound
	}
	st.OccurredAt, st.CreatedAt, st.Voided = st.OccurredAt.UTC(), st.CreatedAt.UTC(), voided != nil
	if err == nil && txID != nil {
		var own string
		if q.QueryRow(ctx, `SELECT user_id::text FROM transactions WHERE id=$1`, *txID).Scan(&own) == nil {
			st.InMyLedger = own == userID
		}
	}
	return st, err
}

func (s *Service) ListSettlements(ctx context.Context, userID, groupID string) ([]Settlement, error) {
	if _, _, err := me(ctx, s.db, userID, groupID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT id::text FROM group_settlements WHERE group_id=$1 ORDER BY occurred_at DESC, id DESC LIMIT 200`, groupID)
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
	out := []Settlement{}
	for _, id := range idList {
		x, err := s.settlement(ctx, s.db, userID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}

func (s *Service) VoidSettlement(ctx context.Context, userID, id string) (Settlement, error) {
	var out Settlement
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		x, err := s.settlement(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		myID, role, err := me(ctx, tx, userID, x.GroupID)
		if err != nil {
			return err
		}
		if role != "admin" && x.From != myID && x.To != myID {
			return ErrForbidden
		}
		if !x.Voided {
			var txID *string
			if err := tx.QueryRow(ctx, `UPDATE group_settlements SET voided_at=now() WHERE id=$1 AND voided_at IS NULL RETURNING transaction_id::text`, id).Scan(&txID); err != nil && !dbx.IsNoRows(err) {
				return err
			}
			if x.InMyLedger && txID != nil {
				if err := s.txs.VoidInTx(ctx, tx, userID, *txID); err != nil {
					return err
				}
			}
			if err := audit.Log(ctx, tx, userID, "group.settlement_voided", map[string]any{"group_id": x.GroupID, "settlement_id": id}); err != nil {
				return err
			}
		}
		out, err = s.settlement(ctx, tx, userID, id)
		return err
	})
	return out, err
}

// MonthTotal is what the group spent in one calendar month (UTC month of the expense date).
type MonthTotal struct {
	Month  string `json:"month"`
	Amount int64  `json:"amount"`
	Count  int    `json:"count"`
}

// GroupAnalytics: totals, each member's contribution (what they paid) and obligation (their share), what is still outstanding,
// and spending by month. Only group records are used; no member's personal transactions are read.
type GroupAnalytics struct {
	Currency      string       `json:"currency"`
	TotalExpenses int64        `json:"total_expenses"`
	TotalSettled  int64        `json:"total_settled"`
	Outstanding   int64        `json:"outstanding"` // what is still to be repaid, in total
	ExpenseCount  int          `json:"expense_count"`
	Members       []Member     `json:"members"`
	Months        []MonthTotal `json:"months"`
}

func (s *Service) Analytics(ctx context.Context, userID, groupID string) (GroupAnalytics, error) {
	b, err := s.balances(ctx, s.db, userID, groupID)
	if err != nil {
		return GroupAnalytics{}, err
	}
	out := GroupAnalytics{Currency: b.Currency, TotalExpenses: b.TotalExpenses, TotalSettled: b.TotalSettled, Members: b.Members, Months: []MonthTotal{}}
	for _, p := range b.Payments {
		out.Outstanding += p.Amount
	}
	rows, err := s.db.Query(ctx, `SELECT to_char(date_trunc('month', occurred_at AT TIME ZONE 'UTC'),'YYYY-MM'), SUM(amount)::bigint, count(*)::int
		FROM group_expenses WHERE group_id=$1 AND voided_at IS NULL GROUP BY 1 ORDER BY 1`, groupID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var m MonthTotal
		if err := rows.Scan(&m.Month, &m.Amount, &m.Count); err != nil {
			return out, err
		}
		out.ExpenseCount += m.Count
		out.Months = append(out.Months, m)
	}
	return out, rows.Err()
}

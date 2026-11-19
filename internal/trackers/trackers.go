// Package trackers owns spending trackers (CLAUDE.md 3.10). A tracker has no limit: it starts at zero and
// accumulates the expenses the user adds to it. Its API deliberately has no limit, remaining or percentage field,
// so no client can render it like a budget.
package trackers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/audit"
	"plinth/server/internal/idempotency"
	"plinth/server/internal/money"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
	"plinth/server/internal/settings"
	"plinth/server/internal/transactions"
)

var ErrVersion = apierr.New(http.StatusConflict, "version_conflict", "This tracker changed since you loaded it. Reload and try again.")

type Tracker struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Notes     string    `json:"notes"`
	Currency  string    `json:"currency"`
	Archived  bool      `json:"archived"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

// Point is the running total at the end of a day.
type Point struct {
	Date  string `json:"date"`
	Total int64  `json:"total"`
}

// View is a tracker with what it has accumulated. There is intentionally no limit, remaining or percentage.
type View struct {
	Tracker
	Total            int64   `json:"total"`
	TransactionCount int     `json:"transaction_count"`
	FirstDate        *string `json:"first_date"`
	LastDate         *string `json:"last_date"`
	Series           []Point `json:"series"` // running total, last 120 days with activity
}

type Input struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Notes    string `json:"notes"`
	Currency string `json:"currency"`
}

type Service struct {
	db       *pgxpool.Pool
	settings *settings.Service
	txs      *transactions.Service
}

func NewService(db *pgxpool.Pool, st *settings.Service, txs *transactions.Service) *Service {
	return &Service{db: db, settings: st, txs: txs}
}

const cols = `t.id::text, t.name, t.notes, t.currency, t.archived_at IS NOT NULL, t.version, t.created_at`

func scan(row interface{ Scan(...any) error }) (Tracker, error) {
	var t Tracker
	err := row.Scan(&t.ID, &t.Name, &t.Notes, &t.Currency, &t.Archived, &t.Version, &t.CreatedAt)
	t.CreatedAt = t.CreatedAt.UTC()
	return t, err
}

func fetch(ctx context.Context, q dbx.Q, userID, id string) (Tracker, error) {
	t, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM trackers t WHERE t.id=$1 AND t.user_id=$2`, id, userID))
	if dbx.IsNoRows(err) {
		return t, apierr.ErrNotFound
	}
	return t, err
}

// view totals a tracker. Eligible: linked expenses that are pending or confirmed (a voided expense drops out).
// Each live refund of a linked expense is subtracted on the expense's day, so a total can never go negative.
func (s *Service) view(ctx context.Context, q dbx.Q, userID string, t Tracker) (View, error) {
	st, err := s.settings.Get(ctx, userID)
	if err != nil {
		return View{}, err
	}
	rows, err := q.Query(ctx, `WITH e AS (
			SELECT x.id, COALESCE(x.counted_amount, x.amount) AS amount, (x.occurred_at AT TIME ZONE $3)::date AS d FROM tracker_transactions k JOIN transactions x ON x.id = k.transaction_id
			WHERE k.tracker_id = $1 AND x.user_id = $2 AND x.type = 'expense' AND x.status IN ('pending','confirmed'))
		SELECT to_char(d,'YYYY-MM-DD'), SUM(delta)::bigint, SUM(n)::int FROM (
			SELECT d, amount AS delta, 1 AS n FROM e
			UNION ALL
			SELECT e.d, -r.amount, 0 FROM e JOIN transactions r ON r.refund_of = e.id AND r.user_id = $2 AND r.status IN ('pending','confirmed')
		) y GROUP BY d ORDER BY d`, t.ID, userID, st.Timezone)
	if err != nil {
		return View{}, err
	}
	defer rows.Close()
	v := View{Tracker: t, Series: []Point{}}
	var run int64
	var points []Point
	for rows.Next() {
		var d string
		var delta int64
		var n int
		if err := rows.Scan(&d, &delta, &n); err != nil {
			return View{}, err
		}
		run += delta
		v.TransactionCount += n
		points = append(points, Point{Date: d, Total: run})
	}
	if err := rows.Err(); err != nil {
		return View{}, err
	}
	v.Total = run
	if len(points) > 0 {
		f, l := points[0].Date, points[len(points)-1].Date
		v.FirstDate, v.LastDate = &f, &l
		if len(points) > 120 {
			points = points[len(points)-120:]
		}
		v.Series = points
	}
	return v, nil
}

func (s *Service) Create(ctx context.Context, userID, key string, in Input) (View, error) {
	if err := idempotency.Check(key, false); err != nil {
		return View{}, err
	}
	f := map[string]string{}
	in.Name, in.Notes = strings.TrimSpace(in.Name), strings.TrimSpace(in.Notes)
	if n := len([]rune(in.Name)); n < 1 || n > 80 {
		f["name"] = "Enter a name of 1 to 80 characters."
	}
	if len([]rune(in.Notes)) > 2000 {
		f["notes"] = "Use 2,000 characters or fewer."
	}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		f["id"] = "Identifier must be a UUID."
	}
	if id == "" {
		id = ids.New()
	}
	cur := strings.ToUpper(in.Currency)
	if cur == "" {
		st, err := s.settings.Get(ctx, userID)
		if err != nil {
			return View{}, err
		}
		cur = st.Currency
	} else if _, ok := money.Lookup(cur); !ok {
		f["currency"] = "Choose a supported currency."
	}
	if len(f) > 0 {
		return View{}, apierr.Validation(f)
	}
	var out View
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "tracker.create", idempotency.Hash("tracker.create", in))
		if err != nil {
			return err
		}
		if replay != nil {
			t, err := fetch(ctx, tx, userID, replay.ResourceID)
			if err != nil {
				return err
			}
			out, err = s.view(ctx, tx, userID, t)
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO trackers (id, user_id, name, notes, currency) VALUES ($1,$2,$3,$4,$5)`, id, userID, in.Name, in.Notes, cur); err != nil {
			if dbx.ConstraintName(err) == "trackers_pkey" {
				return transactions.ErrIDTaken
			}
			return err
		}
		if err := audit.Log(ctx, tx, userID, "tracker.created", map[string]any{"tracker_id": id}); err != nil {
			return err
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "tracker", id); err != nil {
			return err
		}
		t, err := fetch(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		out, err = s.view(ctx, tx, userID, t)
		return err
	})
	return out, err
}

func (s *Service) Get(ctx context.Context, userID, id string) (View, error) {
	t, err := fetch(ctx, s.db, userID, id)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, s.db, userID, t)
}

func (s *Service) List(ctx context.Context, userID string, includeArchived bool) ([]View, error) {
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM trackers t WHERE t.user_id=$1 AND ($2 OR t.archived_at IS NULL) ORDER BY t.archived_at IS NOT NULL, t.created_at DESC, t.id`, userID, includeArchived)
	if err != nil {
		return nil, err
	}
	var ts []Tracker
	for rows.Next() {
		t, err := scan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ts = append(ts, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]View, 0, len(ts))
	for _, t := range ts {
		v, err := s.view(ctx, s.db, userID, t)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

type UpdateInput struct {
	Version int     `json:"version"`
	Name    *string `json:"name"`
	Notes   *string `json:"notes"`
}

func (s *Service) Update(ctx context.Context, userID, id string, in UpdateInput) (View, error) {
	f := map[string]string{}
	if in.Version < 1 {
		f["version"] = "Send the version you loaded."
	}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if c := len([]rune(n)); c < 1 || c > 80 {
			f["name"] = "Enter a name of 1 to 80 characters."
		}
		in.Name = &n
	}
	if in.Notes != nil && len([]rune(*in.Notes)) > 2000 {
		f["notes"] = "Use 2,000 characters or fewer."
	}
	if len(f) > 0 {
		return View{}, apierr.Validation(f)
	}
	tag, err := s.db.Exec(ctx, `UPDATE trackers SET name=COALESCE($4,name), notes=COALESCE($5,notes), version=version+1, updated_at=now()
		WHERE id=$1 AND user_id=$2 AND version=$3`, id, userID, in.Version, in.Name, in.Notes)
	if err != nil {
		return View{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := fetch(ctx, s.db, userID, id); err != nil {
			return View{}, err
		}
		return View{}, ErrVersion
	}
	if err := audit.Log(ctx, s.db, userID, "tracker.updated", map[string]any{"tracker_id": id}); err != nil {
		return View{}, err
	}
	return s.Get(ctx, userID, id)
}

func (s *Service) SetArchived(ctx context.Context, userID, id string, archive bool) (View, error) {
	tag, err := s.db.Exec(ctx, `UPDATE trackers SET archived_at = CASE WHEN $3 THEN COALESCE(archived_at, now()) ELSE NULL END, version=version+1, updated_at=now()
		WHERE id=$1 AND user_id=$2`, id, userID, archive)
	if err != nil {
		return View{}, err
	}
	if tag.RowsAffected() == 0 {
		return View{}, apierr.ErrNotFound
	}
	if err := audit.Log(ctx, s.db, userID, map[bool]string{true: "tracker.archived", false: "tracker.unarchived"}[archive], map[string]any{"tracker_id": id}); err != nil {
		return View{}, err
	}
	return s.Get(ctx, userID, id)
}

// AddTransactions links expenses to a tracker. All or none are added. Only the user's own expenses in the
// tracker's currency qualify; an archived tracker accepts nothing.
func (s *Service) AddTransactions(ctx context.Context, userID, id string, txIDs []string) (View, error) {
	if len(txIDs) == 0 || len(txIDs) > 100 {
		return View{}, apierr.Validation(map[string]string{"transaction_ids": "Choose 1 to 100 transactions."})
	}
	norm := make([]string, 0, len(txIDs))
	for _, x := range txIDs {
		x = ids.Normalize(x)
		if !ids.Valid(x) {
			return View{}, apierr.Validation(map[string]string{"transaction_ids": "One of the transactions is not valid."})
		}
		norm = append(norm, x)
	}
	var out View
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var archived bool
		var cur string
		err := tx.QueryRow(ctx, `SELECT archived_at IS NOT NULL, currency FROM trackers WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, userID).Scan(&archived, &cur)
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if archived {
			return apierr.New(http.StatusConflict, "tracker_archived", "This tracker is archived. Restore it to add transactions.")
		}
		var ok int
		if err := tx.QueryRow(ctx, `SELECT count(DISTINCT id) FROM transactions WHERE id = ANY($1::uuid[]) AND user_id=$2 AND type='expense' AND currency=$3 AND source NOT IN ('reconcile','settlement','topup')`,
			norm, userID, cur).Scan(&ok); err != nil {
			return err
		}
		uniq := map[string]bool{}
		for _, x := range norm {
			uniq[x] = true
		}
		if ok != len(uniq) {
			return apierr.Validation(map[string]string{"transaction_ids": "Only your own expenses in " + cur + " can be added to this tracker."})
		}
		for x := range uniq {
			if _, err := tx.Exec(ctx, `INSERT INTO tracker_transactions (tracker_id, transaction_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, x); err != nil {
				return err
			}
		}
		if err := audit.Log(ctx, tx, userID, "tracker.transactions_added", map[string]any{"tracker_id": id, "count": len(uniq)}); err != nil {
			return err
		}
		t, err := fetch(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		out, err = s.view(ctx, tx, userID, t)
		return err
	})
	return out, err
}

// RemoveTransaction unlinks one transaction. The transaction itself is untouched.
func (s *Service) RemoveTransaction(ctx context.Context, userID, id, txID string) (View, error) {
	if _, err := fetch(ctx, s.db, userID, id); err != nil {
		return View{}, err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM tracker_transactions WHERE tracker_id=$1 AND transaction_id=$2`, id, txID); err != nil {
		return View{}, err
	}
	return s.Get(ctx, userID, id)
}

// Transactions lists the transactions added to a tracker, newest first, using the shared history query.
func (s *Service) Transactions(ctx context.Context, userID, id string, p transactions.ListParams) (transactions.Page, error) {
	if _, err := fetch(ctx, s.db, userID, id); err != nil {
		return transactions.Page{}, err
	}
	p.TrackerID = id
	return s.txs.List(ctx, userID, p)
}

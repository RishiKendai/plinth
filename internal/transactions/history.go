package transactions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/ids"
)

// ListParams filters and orders the history. Zero values mean "no filter".
type ListParams struct {
	AccountID     string
	CategoryID    string // matches the category and its subcategories
	Type          string
	Status        string
	PaymentMethod string
	Tag           string
	TrackerID     string // only transactions added to this spending tracker
	Q             string // text search over title, description, notes, names, tags, category and account
	From, To      string // RFC 3339, or YYYY-MM-DD in the user's time zone (To is inclusive for a date)
	MinAmount     *int64
	MaxAmount     *int64
	Sort          string // date (default) | amount
	Asc           bool
	Limit         int
	Cursor        string
}

type Page struct {
	Items      []Transaction
	NextCursor string
}

var errBadCursor = apierr.New(http.StatusBadRequest, "bad_cursor", "That page marker is not valid. Start again from the first page.")

type cursor struct {
	K  string `json:"k"`
	ID string `json:"id"`
}

func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursor, error) {
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, &c) != nil || !ids.Valid(c.ID) || c.K == "" {
		return c, errBadCursor
	}
	return c, nil
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// prefixQuery turns "swig dinn" into "swig:* & dinn:*" for to_tsquery, dropping punctuation.
func prefixQuery(q string) string {
	var toks []string
	for _, w := range strings.FieldsFunc(q, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		toks = append(toks, strings.ToLower(w)+":*")
	}
	return strings.Join(toks, " & ")
}

func (s *Service) resolveTime(v string, loc *time.Location, endOfDate bool) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		u := t.UTC()
		return &u, nil
	}
	if d, err := time.ParseInLocation("2006-01-02", v, loc); err == nil {
		if endOfDate {
			d = d.AddDate(0, 0, 1)
		}
		u := d.UTC()
		return &u, nil
	}
	return nil, apierr.Validation(map[string]string{"date": "Use a date like 2026-11-18 or a full timestamp."})
}

// List returns one page of the user's history. Sorting, filtering and paging all happen in PostgreSQL;
// pages use a keyset cursor so deep pages cost the same as the first. A transfer shows once (its outgoing leg)
// unless the list is filtered to an account, where each account sees its own leg.
func (s *Service) List(ctx context.Context, userID string, p ListParams) (Page, error) {
	f := map[string]string{}
	if p.Type != "" && p.Type != "expense" && p.Type != "income" && p.Type != "transfer" {
		f["type"] = "Choose expense, income or transfer."
	}
	if p.Status != "" && transitions[p.Status] == nil && p.Status != "failed" && p.Status != "cancelled" && p.Status != "reversed" {
		f["status"] = "Choose a valid status."
	}
	if p.PaymentMethod != "" && !paymentMethods[p.PaymentMethod] {
		f["payment_method"] = "Choose a valid payment method."
	}
	for name, v := range map[string]string{"account_id": p.AccountID, "category_id": p.CategoryID, "tracker_id": p.TrackerID} {
		if v != "" && !ids.Valid(v) {
			f[name] = "Not a valid identifier."
		}
	}
	if p.Sort == "" {
		p.Sort = "date"
	}
	if p.Sort != "date" && p.Sort != "amount" {
		f["sort"] = "Sort by date or amount."
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.Limit > 200 {
		p.Limit = 200
	}
	if len(f) > 0 {
		return Page{}, apierr.Validation(f)
	}
	st, err := s.settings.Get(ctx, userID)
	if err != nil {
		return Page{}, err
	}
	loc, lerr := time.LoadLocation(st.Timezone)
	if lerr != nil {
		loc = time.UTC
	}
	from, err := s.resolveTime(p.From, loc, false)
	if err != nil {
		return Page{}, err
	}
	to, err := s.resolveTime(p.To, loc, true)
	if err != nil {
		return Page{}, err
	}

	args := []any{userID}
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	where := []string{"t.user_id = $1", "t.deleted_at IS NULL"} // deleted (hidden) rows never appear in history or search
	if p.AccountID == "" {
		where = append(where, "(t.type <> 'transfer' OR t.transfer_direction = 'out')")
	} else {
		where = append(where, "t.account_id = "+arg(p.AccountID))
	}
	if p.CategoryID != "" {
		a := arg(p.CategoryID)
		where = append(where, "(t.category_id = "+a+" OR t.category_id IN (SELECT id FROM categories WHERE parent_id = "+a+"))")
	}
	if p.Type != "" {
		where = append(where, "t.type = "+arg(p.Type))
	}
	if p.Status != "" {
		where = append(where, "t.status = "+arg(p.Status))
	}
	if p.PaymentMethod != "" {
		where = append(where, "t.payment_method = "+arg(p.PaymentMethod))
	}
	if p.TrackerID != "" {
		where = append(where, "EXISTS (SELECT 1 FROM tracker_transactions k WHERE k.transaction_id = t.id AND k.tracker_id = "+arg(p.TrackerID)+")")
	}
	if p.Tag != "" {
		where = append(where, "EXISTS (SELECT 1 FROM transaction_tags x JOIN tags g ON g.id = x.tag_id WHERE x.transaction_id = t.id AND lower(g.name) = lower("+arg(p.Tag)+"))")
	}
	if from != nil {
		where = append(where, "t.occurred_at >= "+arg(*from))
	}
	if to != nil {
		where = append(where, "t.occurred_at < "+arg(*to))
	}
	if p.MinAmount != nil {
		where = append(where, "t.amount >= "+arg(*p.MinAmount))
	}
	if p.MaxAmount != nil {
		where = append(where, "t.amount <= "+arg(*p.MaxAmount))
	}
	if q := strings.TrimSpace(p.Q); q != "" {
		like := arg("%" + escapeLike(q) + "%")
		parts := []string{
			"t.title ILIKE " + like, "t.payee_name ILIKE " + like, "t.payer_name ILIKE " + like,
			"t.notes ILIKE " + like, "t.description ILIKE " + like,
			"t.title % " + arg(q), // trigram similarity: tolerates small typos
			"EXISTS (SELECT 1 FROM transaction_tags x JOIN tags g ON g.id = x.tag_id WHERE x.transaction_id = t.id AND g.name ILIKE " + like + ")",
			"EXISTS (SELECT 1 FROM categories c WHERE c.id = t.category_id AND c.name ILIKE " + like + ")",
			"EXISTS (SELECT 1 FROM accounts a WHERE a.id = t.account_id AND a.name ILIKE " + like + ")",
		}
		if tq := prefixQuery(q); tq != "" {
			parts = append(parts, "t.search @@ to_tsquery('simple', "+arg(tq)+")")
		}
		where = append(where, "("+strings.Join(parts, " OR ")+")")
	}

	// Keyset paging on (sort key, id) in the chosen direction.
	key := "t.occurred_at"
	if p.Sort == "amount" {
		key = "t.amount"
	}
	op, dir := "<", "DESC"
	if p.Asc {
		op, dir = ">", "ASC"
	}
	if p.Cursor != "" {
		c, err := decodeCursor(p.Cursor)
		if err != nil {
			return Page{}, err
		}
		var kv any
		if p.Sort == "amount" {
			n, perr := strconv.ParseInt(c.K, 10, 64)
			if perr != nil {
				return Page{}, errBadCursor
			}
			kv = n
		} else {
			t, perr := time.Parse(time.RFC3339Nano, c.K)
			if perr != nil {
				return Page{}, errBadCursor
			}
			kv = t
		}
		where = append(where, fmt.Sprintf("(%s, t.id) %s (%s, %s::uuid)", key, op, arg(kv), arg(c.ID)))
	}
	sql := fmt.Sprintf(`SELECT %s FROM transactions t WHERE %s ORDER BY %s %s, t.id %s LIMIT %d`,
		txCols, strings.Join(where, " AND "), key, dir, dir, p.Limit+1)
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	var items []Transaction
	for rows.Next() {
		t, err := scanTx(rows)
		if err != nil {
			return Page{}, err
		}
		items = append(items, t)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	page := Page{Items: items}
	if len(items) > p.Limit {
		page.Items = items[:p.Limit]
		last := page.Items[p.Limit-1]
		k := last.OccurredAt.Format(time.RFC3339Nano)
		if p.Sort == "amount" {
			k = strconv.FormatInt(last.Amount, 10)
		}
		page.NextCursor = encodeCursor(cursor{K: k, ID: last.ID})
	}
	if page.Items == nil {
		page.Items = []Transaction{}
	}
	return page, nil
}

// SyncSettle is how far behind "now" a sync pull stops. updated_at is the transaction's start time, so a long-running
// write could commit after a later row was already pulled; holding the cursor back by more than the longest request
// (REQUEST_TIMEOUT, 15 s by default) means no row can appear behind the cursor. Docs: docs/sync.md.
var SyncSettle = 30 * time.Second // a var only so tests can shorten it

// Changes returns transactions changed after the cursor, oldest first, for incremental sync. Rows are never removed (a
// void is a status change and a delete only sets deleted_at, which devices act on), so "changed since" is complete. hasMore tells the caller to ask again immediately.
func (s *Service) Changes(ctx context.Context, userID, cur string, limit int) (items []Transaction, next string, hasMore bool, err error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var kv, id any
	if cur != "" {
		c, derr := decodeCursor(cur)
		if derr != nil {
			return nil, "", false, derr
		}
		t, perr := time.Parse(time.RFC3339Nano, c.K)
		if perr != nil {
			return nil, "", false, errBadCursor
		}
		kv, id = t, c.ID
	}
	rows, err := s.db.Query(ctx, `SELECT `+txCols+` FROM transactions t
		WHERE t.user_id = $1 AND ($2::timestamptz IS NULL OR (t.updated_at, t.id) > ($2::timestamptz, $3::uuid)) AND t.updated_at < now() - $5::interval
		ORDER BY t.updated_at, t.id LIMIT $4`, userID, kv, id, limit+1, SyncSettle.String())
	if err != nil {
		return nil, "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanTx(rows)
		if err != nil {
			return nil, "", false, err
		}
		items = append(items, t)
	}
	if err := rows.Err(); err != nil {
		return nil, "", false, err
	}
	if len(items) > limit {
		items, hasMore = items[:limit], true
	}
	if items == nil {
		items = []Transaction{}
	}
	next = cur
	if n := len(items); n > 0 {
		next = encodeCursor(cursor{K: items[n-1].UpdatedAt.Format(time.RFC3339Nano), ID: items[n-1].ID})
	}
	return items, next, hasMore, nil
}

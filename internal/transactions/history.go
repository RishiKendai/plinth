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
	for name, v := range map[string]string{"account_id": p.AccountID, "category_id": p.CategoryID} {
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
	where := []string{"t.user_id = $1"}
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

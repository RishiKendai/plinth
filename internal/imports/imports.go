// Package imports owns CSV import and export (CLAUDE.md 3.13).
//
// Flow: upload the file, map its columns, review the preview (ready, duplicate and invalid rows, debit and credit
// totals), confirm, and optionally undo. Nothing becomes a transaction until the user confirms, invalid rows are
// never imported, and every import runs through the transactions module like any other source. Re-importing the same
// file cannot duplicate anything: each row carries a source-event reference that the database makes unique.
package imports

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/accounts"
	"plinth/server/internal/audit"
	"plinth/server/internal/categories"
	"plinth/server/internal/money"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
	"plinth/server/internal/settings"
	"plinth/server/internal/transactions"
)

const (
	MaxContent = 5 << 20 // 5 MiB of CSV text
	MaxRows    = 5000
	previewMax = 200
)

var (
	ErrState = apierr.New(http.StatusConflict, "invalid_state", "That import is not in a state that allows this.")
	ErrEmpty = apierr.Validation(map[string]string{"content": "The file has no data rows."})
)

type Mapping struct {
	Date            string `json:"date"`
	Title           string `json:"title"`
	Amount          string `json:"amount"` // one signed column, or use Debit and Credit
	Debit           string `json:"debit"`
	Credit          string `json:"credit"`
	Direction       string `json:"direction"` // optional Dr/Cr column
	Reference       string `json:"reference"`
	Category        string `json:"category"`
	DateFormat      string `json:"date_format"`       // dmy (default), mdy, ymd
	PositiveIsDebit bool   `json:"positive_is_debit"` // single amount column: by default a minus sign means money out
}

type Batch struct {
	ID            string     `json:"id"`
	AccountID     string     `json:"account_id"`
	Filename      string     `json:"filename"`
	Status        string     `json:"status"` // uploaded | mapped | confirmed | undone
	Headers       []string   `json:"headers,omitempty"`
	Sample        [][]string `json:"sample,omitempty"`
	Suggested     *Mapping   `json:"suggested_mapping,omitempty"`
	Mapping       *Mapping   `json:"mapping"`
	TotalRows     int        `json:"total_rows"`
	ValidRows     int        `json:"valid_rows"`
	InvalidRows   int        `json:"invalid_rows"`
	DuplicateRows int        `json:"duplicate_rows"`
	TotalDebit    int64      `json:"total_debit"`
	TotalCredit   int64      `json:"total_credit"`
	CreatedCount  int        `json:"created_count"`
	CreatedAt     time.Time  `json:"created_at"`
	ConfirmedAt   *time.Time `json:"confirmed_at"`
}

type Row struct {
	RowNo     int    `json:"row"`
	Date      string `json:"date"`
	Title     string `json:"title"`
	Amount    int64  `json:"amount"`
	Direction string `json:"direction"`
	Reference string `json:"reference"`
	Category  string `json:"category"`
	Status    string `json:"status"` // valid | invalid | duplicate
	Error     string `json:"error"`
}

type Service struct {
	db       *pgxpool.Pool
	settings *settings.Service
	cats     *categories.Service
	txs      *transactions.Service
	now      func() time.Time
}

func NewService(db *pgxpool.Pool, st *settings.Service, cats *categories.Service, txs *transactions.Service) *Service {
	return &Service{db: db, settings: st, cats: cats, txs: txs, now: time.Now}
}

// ---- CSV reading ----

func readCSV(content string) ([][]string, error) {
	content = strings.TrimPrefix(content, "\ufeff")
	first := content
	if i := strings.IndexAny(content, "\r\n"); i >= 0 {
		first = content[:i]
	}
	delim := ','
	for _, d := range []rune{';', '\t', '|'} {
		if strings.Count(first, string(d)) > strings.Count(first, string(delim)) {
			delim = d
		}
	}
	r := csv.NewReader(strings.NewReader(content))
	r.Comma = delim
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true
	var rows [][]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, apierr.Validation(map[string]string{"content": "The file is not valid CSV. Check it opens correctly in a spreadsheet."})
		}
		blank := true
		for _, c := range rec {
			if strings.TrimSpace(c) != "" {
				blank = false
			}
		}
		if !blank {
			rows = append(rows, rec)
		}
		if len(rows) > MaxRows+1 {
			return nil, apierr.Validation(map[string]string{"content": fmt.Sprintf("Import at most %d rows at a time. Split the file and import it in parts.", MaxRows)})
		}
	}
	return rows, nil
}

// guess proposes a column mapping from common header names. It is only a suggestion; the user confirms it.
func guess(headers []string) Mapping {
	m := Mapping{DateFormat: "dmy"}
	find := func(words ...string) string {
		for _, h := range headers {
			l := strings.ToLower(h)
			for _, w := range words {
				if strings.Contains(l, w) {
					return h
				}
			}
		}
		return ""
	}
	m.Date = find("date")
	m.Title = find("description", "narration", "particulars", "details", "merchant", "title", "payee")
	m.Debit = find("debit", "withdrawal", "paid out")
	m.Credit = find("credit", "deposit", "paid in")
	if m.Debit == "" || m.Credit == "" {
		m.Debit, m.Credit = "", ""
		m.Amount = find("amount")
	}
	m.Reference = find("reference", "ref no", "utr", "txn id", "transaction id", "cheque")
	m.Category = find("category")
	return m
}

func colIndex(headers []string) map[string]int {
	out := map[string]int{}
	for i, h := range headers {
		if _, dup := out[h]; !dup {
			out[h] = i
		}
	}
	return out
}

// ---- upload ----

type UploadInput struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Filename  string `json:"filename"`
	Content   string `json:"content"`
}

func (s *Service) Upload(ctx context.Context, userID string, in UploadInput) (Batch, error) {
	f := map[string]string{}
	if in.Content == "" || len(in.Content) > MaxContent {
		f["content"] = "Choose a CSV file of up to 5 MB."
	}
	aid := ids.Normalize(in.AccountID)
	if !ids.Valid(aid) {
		f["account_id"] = "Choose the account these transactions belong to."
	}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		f["id"] = "Identifier must be a UUID."
	}
	if utf8.RuneCountInString(in.Filename) > 200 {
		f["filename"] = "The file name is too long."
	}
	if len(f) > 0 {
		return Batch{}, apierr.Validation(f)
	}
	if id == "" {
		id = ids.New()
	}
	acct, err := accounts.Fetch(ctx, s.db, userID, aid)
	if err != nil || acct.ArchivedAt != nil {
		return Batch{}, apierr.Validation(map[string]string{"account_id": "Choose one of your active accounts."})
	}
	rows, err := readCSV(in.Content)
	if err != nil {
		return Batch{}, err
	}
	if len(rows) < 2 {
		return Batch{}, ErrEmpty
	}
	// An upload with the same client id is the same import: return it instead of creating another.
	if b, err := s.get(ctx, s.db, userID, id); err == nil {
		return b, nil
	}
	_, err = s.db.Exec(ctx, `INSERT INTO import_batches (id, user_id, account_id, filename, content, total_rows) VALUES ($1,$2,$3,$4,$5,$6)`,
		id, userID, aid, in.Filename, in.Content, len(rows)-1)
	if err != nil {
		if dbx.ConstraintName(err) == "import_batches_pkey" {
			return Batch{}, apierr.New(http.StatusConflict, "id_conflict", "That identifier cannot be used. Generate a new one.")
		}
		return Batch{}, err
	}
	_ = audit.Log(ctx, s.db, userID, "import.uploaded", map[string]any{"batch_id": id, "rows": len(rows) - 1})
	b, err := s.get(ctx, s.db, userID, id)
	if err != nil {
		return b, err
	}
	b.Headers = rows[0]
	for i := 1; i < len(rows) && i <= 5; i++ {
		b.Sample = append(b.Sample, rows[i])
	}
	g := guess(rows[0])
	b.Suggested = &g
	return b, nil
}

const batchCols = `id::text, account_id::text, filename, status, mapping, total_rows, valid_rows, invalid_rows, duplicate_rows, total_debit, total_credit, created_count, created_at, confirmed_at`

func (s *Service) get(ctx context.Context, q dbx.Q, userID, id string) (Batch, error) {
	var b Batch
	var raw []byte
	err := q.QueryRow(ctx, `SELECT `+batchCols+` FROM import_batches WHERE id=$1 AND user_id=$2`, id, userID).
		Scan(&b.ID, &b.AccountID, &b.Filename, &b.Status, &raw, &b.TotalRows, &b.ValidRows, &b.InvalidRows, &b.DuplicateRows, &b.TotalDebit, &b.TotalCredit, &b.CreatedCount, &b.CreatedAt, &b.ConfirmedAt)
	if dbx.IsNoRows(err) {
		return b, apierr.ErrNotFound
	}
	if err != nil {
		return b, err
	}
	b.CreatedAt = b.CreatedAt.UTC()
	if len(raw) > 0 {
		var m Mapping
		if jsonUnmarshal(raw, &m) == nil {
			b.Mapping = &m
		}
	}
	return b, nil
}

func (s *Service) Get(ctx context.Context, userID, id string) (Batch, error) {
	return s.get(ctx, s.db, userID, id)
}

func (s *Service) List(ctx context.Context, userID string) ([]Batch, error) {
	rows, err := s.db.Query(ctx, `SELECT id::text FROM import_batches WHERE user_id=$1 ORDER BY created_at DESC LIMIT 50`, userID)
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
	out := []Batch{}
	for _, id := range idList {
		b, err := s.get(ctx, s.db, userID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// ---- mapping and validation ----

type parsedRow struct {
	Row
	amountSet bool
	ref       string
	fp        string
}

func cell(rec []string, idx map[string]int, col string) string {
	if col == "" {
		return ""
	}
	i, ok := idx[col]
	if !ok || i >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[i])
}

func directionOf(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "dr", "debit", "d", "withdrawal", "out", "expense":
		return "debit"
	case "cr", "credit", "c", "deposit", "in", "income":
		return "credit"
	}
	return ""
}

func parseRow(rec []string, idx map[string]int, m Mapping, exponent int, no int) parsedRow {
	r := parsedRow{Row: Row{RowNo: no, Status: "valid"}}
	fail := func(msg string) parsedRow { r.Status, r.Error = "invalid", msg; return r }
	d, err := ParseDate(cell(rec, idx, m.Date), m.DateFormat)
	if err != nil {
		return fail("Date is not valid: " + strconv.Quote(cell(rec, idx, m.Date)) + ".")
	}
	r.Date = d.Format("2006-01-02")
	r.Title = cell(rec, idx, m.Title)
	r.Reference = cell(rec, idx, m.Reference)
	r.Category = cell(rec, idx, m.Category)
	if r.Title == "" {
		r.Title = r.Reference
	}
	if r.Title == "" {
		return fail("Description is empty.")
	}
	if utf8.RuneCountInString(r.Title) > 120 {
		rs := []rune(r.Title)
		r.Title = string(rs[:120])
	}
	if utf8.RuneCountInString(r.Reference) > 100 {
		return fail("Reference is too long.")
	}
	if m.Debit != "" && m.Credit != "" {
		dv, cv := cell(rec, idx, m.Debit), cell(rec, idx, m.Credit)
		var da, ca int64
		if dv != "" {
			a, _, _, err := ParseAmount(dv, exponent)
			if err != nil {
				return fail("Debit amount is not valid: " + err.Error() + ".")
			}
			da = a
		}
		if cv != "" {
			a, _, _, err := ParseAmount(cv, exponent)
			if err != nil {
				return fail("Credit amount is not valid: " + err.Error() + ".")
			}
			ca = a
		}
		switch {
		case da > 0 && ca > 0:
			return fail("Both a debit and a credit are filled in.")
		case da > 0:
			r.Amount, r.Direction = da, "debit"
		case ca > 0:
			r.Amount, r.Direction = ca, "credit"
		default:
			return fail("No amount.")
		}
		return r
	}
	a, neg, dir, err := ParseAmount(cell(rec, idx, m.Amount), exponent)
	if err != nil {
		return fail("Amount is not valid: " + err.Error() + ".")
	}
	if a <= 0 {
		return fail("Amount must be greater than zero.")
	}
	if dir == "" {
		dir = directionOf(cell(rec, idx, m.Direction))
	}
	if dir == "" {
		// No marker: the sign decides. By default a minus sign means money out.
		if neg != m.PositiveIsDebit {
			dir = "debit"
		} else {
			dir = "credit"
		}
	}
	r.Amount, r.Direction = a, dir
	return r
}

func fingerprint(accountID string, p parsedRow, occurrence int) string {
	h := sha256.Sum256([]byte(strings.Join([]string{accountID, p.Date, p.Direction, strconv.FormatInt(p.Amount, 10), Normalize(p.Title), p.ref, strconv.Itoa(occurrence)}, "|")))
	return hex.EncodeToString(h[:])
}

// extRef is the source-event reference stored on the transaction. A bank reference identifies the event across
// files; without one, a fingerprint of the row (plus its position among identical rows) does.
func extRef(accountID string, p parsedRow, fp string) string {
	if p.Reference != "" {
		return "csv:ref:" + accountID + ":" + p.Reference
	}
	return "csv:fp:" + fp
}

func (s *Service) loc(userID string) (*time.Location, string, error) {
	st, err := s.settings.Get(ctx0, userID)
	if err != nil {
		return nil, "", err
	}
	l, lerr := time.LoadLocation(st.Timezone)
	if lerr != nil {
		l = time.UTC
	}
	return l, st.Timezone, nil
}

var ctx0 = context.Background()

// SetMapping parses and validates every row with the chosen columns. It can be called again to change the mapping.
func (s *Service) SetMapping(ctx context.Context, userID, id string, m Mapping) (Batch, error) {
	var content, accountID, status string
	err := s.db.QueryRow(ctx, `SELECT content, account_id::text, status FROM import_batches WHERE id=$1 AND user_id=$2`, id, userID).Scan(&content, &accountID, &status)
	if dbx.IsNoRows(err) {
		return Batch{}, apierr.ErrNotFound
	}
	if err != nil {
		return Batch{}, err
	}
	if status != "uploaded" && status != "mapped" {
		return Batch{}, ErrState
	}
	rows, err := readCSV(content)
	if err != nil {
		return Batch{}, err
	}
	headers := rows[0]
	idx := colIndex(headers)
	f := map[string]string{}
	need := func(field, col string, required bool) {
		if col == "" {
			if required {
				f[field] = "Choose the column that holds this."
			}
			return
		}
		if _, ok := idx[col]; !ok {
			f[field] = "That column is not in the file."
		}
	}
	need("date", m.Date, true)
	need("title", m.Title, false)
	need("reference", m.Reference, false)
	need("category", m.Category, false)
	need("direction", m.Direction, false)
	if m.Amount == "" && (m.Debit == "" || m.Credit == "") {
		f["amount"] = "Choose an amount column, or both a debit and a credit column."
	} else if m.Amount != "" {
		need("amount", m.Amount, true)
		m.Debit, m.Credit = "", ""
	} else {
		need("debit", m.Debit, true)
		need("credit", m.Credit, true)
	}
	if m.Title == "" && m.Reference == "" {
		f["title"] = "Choose a description column (or a reference column to use instead)."
	}
	if m.DateFormat != "" && m.DateFormat != "dmy" && m.DateFormat != "mdy" && m.DateFormat != "ymd" {
		f["date_format"] = "Choose dmy, mdy or ymd."
	}
	if len(f) > 0 {
		return Batch{}, apierr.Validation(f)
	}
	if m.DateFormat == "" {
		m.DateFormat = "dmy"
	}
	acct, err := accounts.Fetch(ctx, s.db, userID, accountID)
	if err != nil {
		return Batch{}, err
	}
	cur, _ := money.Lookup(acct.Currency)
	loc, tzName, err := s.loc(userID)
	if err != nil {
		return Batch{}, err
	}

	parsed := make([]parsedRow, 0, len(rows)-1)
	seen := map[string]int{}
	var minD, maxD time.Time
	for i, rec := range rows[1:] {
		p := parseRow(rec, idx, m, cur.Exponent, i+2) // row numbers match the spreadsheet: header is row 1
		p.ref = p.Reference
		if p.Status == "valid" {
			base := fingerprint(accountID, p, 0)
			occ := seen[base]
			seen[base]++
			p.fp = fingerprint(accountID, p, occ)
			d, _ := time.Parse("2006-01-02", p.Date)
			if minD.IsZero() || d.Before(minD) {
				minD = d
			}
			if d.After(maxD) {
				maxD = d
			}
		}
		parsed = append(parsed, p)
	}

	// Duplicate candidates: already imported (same source reference) or matching an existing transaction on the
	// same account by date, amount, direction and description.
	exist := map[string][]string{}
	refs := map[string]bool{}
	if !minD.IsZero() {
		from := time.Date(minD.Year(), minD.Month(), minD.Day()-1, 0, 0, 0, 0, loc).UTC()
		to := time.Date(maxD.Year(), maxD.Month(), maxD.Day()+2, 0, 0, 0, 0, loc).UTC()
		er, err := s.db.Query(ctx, `SELECT type, amount, to_char(occurred_at AT TIME ZONE $4,'YYYY-MM-DD'), title, external_ref FROM transactions
			WHERE user_id=$1 AND account_id=$2 AND status IN ('pending','confirmed') AND type IN ('expense','income') AND occurred_at >= $3 AND occurred_at < $5`,
			userID, accountID, from, tzName, to)
		if err != nil {
			return Batch{}, err
		}
		for er.Next() {
			var typ, d, title, ref string
			var amt int64
			if err := er.Scan(&typ, &amt, &d, &title, &ref); err != nil {
				er.Close()
				return Batch{}, err
			}
			dir := "debit"
			if typ == "income" {
				dir = "credit"
			}
			k := d + "|" + dir + "|" + strconv.FormatInt(amt, 10)
			exist[k] = append(exist[k], Normalize(title))
			if ref != "" {
				refs[ref] = true
			}
		}
		er.Close()
		if err := er.Err(); err != nil {
			return Batch{}, err
		}
	}
	// Source references of already-imported rows may be outside the date window; check them directly.
	var candidate []string
	for i := range parsed {
		if parsed[i].Status == "valid" {
			candidate = append(candidate, extRef(accountID, parsed[i], parsed[i].fp))
		}
	}
	if len(candidate) > 0 {
		rr, err := s.db.Query(ctx, `SELECT external_ref FROM transactions WHERE user_id=$1 AND source='csv' AND status IN ('pending','confirmed') AND external_ref = ANY($2)`, userID, candidate)
		if err != nil {
			return Batch{}, err
		}
		for rr.Next() {
			var r string
			if err := rr.Scan(&r); err != nil {
				rr.Close()
				return Batch{}, err
			}
			refs[r] = true
		}
		rr.Close()
	}
	var valid, invalid, dup int
	var debit, credit int64
	for i := range parsed {
		p := &parsed[i]
		if p.Status == "valid" {
			if refs[extRef(accountID, *p, p.fp)] {
				p.Status, p.Error = "duplicate", "Already imported."
			} else {
				for _, t := range exist[p.Date+"|"+p.Direction+"|"+strconv.FormatInt(p.Amount, 10)] {
					nt := Normalize(p.Title)
					if t == nt || (len(nt) > 3 && (strings.Contains(t, nt) || strings.Contains(nt, t))) {
						p.Status, p.Error = "duplicate", "Looks like a transaction you already have."
						break
					}
				}
			}
		}
		switch p.Status {
		case "valid":
			valid++
			if p.Direction == "debit" {
				debit += p.Amount
			} else {
				credit += p.Amount
			}
		case "duplicate":
			dup++
		default:
			invalid++
		}
	}

	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM import_batches WHERE id=$1 FOR UPDATE`, id); err != nil {
			return err
		}
		var cur string
		if err := tx.QueryRow(ctx, `SELECT status FROM import_batches WHERE id=$1`, id).Scan(&cur); err != nil {
			return err
		}
		if cur != "uploaded" && cur != "mapped" {
			return ErrState
		}
		if _, err := tx.Exec(ctx, `DELETE FROM import_rows WHERE batch_id=$1`, id); err != nil {
			return err
		}
		b := &pgx.Batch{}
		for _, p := range parsed {
			var on any
			if p.Date != "" {
				on = p.Date
			}
			b.Queue(`INSERT INTO import_rows (batch_id, row_no, occurred_on, title, amount, direction, reference, category, status, error, fingerprint)
				VALUES ($1,$2,$3::date,$4,$5,$6,$7,$8,$9,$10,$11)`, id, p.RowNo, on, p.Title, p.Amount, p.Direction, p.Reference, p.Category, p.Status, p.Error, p.fp)
		}
		br := tx.SendBatch(ctx, b)
		for range parsed {
			if _, err := br.Exec(); err != nil {
				br.Close()
				return err
			}
		}
		if err := br.Close(); err != nil {
			return err
		}
		mj, _ := jsonMarshal(m)
		_, err := tx.Exec(ctx, `UPDATE import_batches SET status='mapped', mapping=$2::jsonb, total_rows=$3, valid_rows=$4, invalid_rows=$5, duplicate_rows=$6, total_debit=$7, total_credit=$8 WHERE id=$1`,
			id, string(mj), len(parsed), valid, invalid, dup, debit, credit)
		return err
	})
	if err != nil {
		return Batch{}, err
	}
	return s.get(ctx, s.db, userID, id)
}

func (s *Service) Rows(ctx context.Context, userID, id, status string, limit int) (Batch, []Row, error) {
	b, err := s.get(ctx, s.db, userID, id)
	if err != nil {
		return b, nil, err
	}
	if limit <= 0 || limit > previewMax {
		limit = 50
	}
	rows, err := s.db.Query(ctx, `SELECT row_no, COALESCE(to_char(occurred_on,'YYYY-MM-DD'),''), title, amount, direction, reference, category, status, error FROM import_rows
		WHERE batch_id=$1 AND ($2 = '' OR status = $2) ORDER BY row_no LIMIT $3`, id, status, limit)
	if err != nil {
		return b, nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.RowNo, &r.Date, &r.Title, &r.Amount, &r.Direction, &r.Reference, &r.Category, &r.Status, &r.Error); err != nil {
			return b, nil, err
		}
		out = append(out, r)
	}
	return b, out, rows.Err()
}

// ---- confirm and undo ----

// Confirm creates the transactions for every valid row (and any duplicate rows the user chose to include) in one
// database transaction. It is idempotent: confirming an already-confirmed batch returns it unchanged, and a row whose
// source reference already backs a live transaction is skipped, never duplicated.
func (s *Service) Confirm(ctx context.Context, userID, id string, includeDuplicates []int) (Batch, error) {
	var out Batch
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var status, accountID string
		err := tx.QueryRow(ctx, `SELECT status, account_id::text FROM import_batches WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, userID).Scan(&status, &accountID)
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if status == "confirmed" {
			out, err = s.get(ctx, tx, userID, id)
			return err
		}
		if status != "mapped" {
			return ErrState
		}
		loc, tzName, err := s.loc(userID)
		if err != nil {
			return err
		}
		inc := map[int]bool{}
		for _, n := range includeDuplicates {
			inc[n] = true
		}
		catList, err := s.cats.List(ctx, userID, "", false)
		if err != nil {
			return err
		}
		catByKind := map[string]map[string]string{"expense": {}, "income": {}}
		for _, c := range catList {
			catByKind[c.Kind][strings.ToLower(c.Name)] = c.ID
		}
		rows, err := tx.Query(ctx, `SELECT row_no, COALESCE(to_char(occurred_on,'YYYY-MM-DD'),''), title, amount, direction, reference, category, status, fingerprint FROM import_rows WHERE batch_id=$1 ORDER BY row_no`, id)
		if err != nil {
			return err
		}
		type rec struct {
			no                    int
			date, title, dir, ref string
			category, status, fp  string
			amount                int64
		}
		var todo []rec
		for rows.Next() {
			var r rec
			if err := rows.Scan(&r.no, &r.date, &r.title, &r.amount, &r.dir, &r.ref, &r.category, &r.status, &r.fp); err != nil {
				rows.Close()
				return err
			}
			if r.status == "valid" || (r.status == "duplicate" && inc[r.no]) {
				todo = append(todo, r)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		created := 0
		for _, r := range todo {
			d, _ := time.Parse("2006-01-02", r.date)
			typ := "expense"
			if r.dir == "credit" {
				typ = "income"
			}
			n := transactions.NewTx{AccountID: accountID, Type: typ, Amount: r.amount, Title: r.title, Status: "confirmed",
				OccurredAt: time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, loc).UTC(), Timezone: tzName, Source: "csv",
				ExternalRef: extRef(accountID, parsedRow{Row: Row{Reference: r.ref}}, r.fp)}
			if cid, ok := catByKind[typ][strings.ToLower(r.category)]; ok && r.category != "" {
				n.CategoryID = &cid
			}
			if r.status == "duplicate" {
				// The user chose to import a probable duplicate: give it its own reference so the source-event
				// index does not suppress it, while staying unique per batch and row.
				n.ExternalRef = "csv:dup:" + id + ":" + strconv.Itoa(r.no)
			}
			sp, err := tx.Begin(ctx) // savepoint: one skipped row must not abort the batch
			if err != nil {
				return err
			}
			txID, err := s.txs.CreateInTx(ctx, sp, userID, n)
			if err != nil {
				_ = sp.Rollback(ctx)
				if err == transactions.ErrDuplicateEvent {
					_, _ = tx.Exec(ctx, `UPDATE import_rows SET status='duplicate', error='Already imported.' WHERE batch_id=$1 AND row_no=$2`, id, r.no)
					continue
				}
				_, _ = tx.Exec(ctx, `UPDATE import_rows SET status='invalid', error=$3 WHERE batch_id=$1 AND row_no=$2`, id, r.no, errorText(err))
				continue
			}
			if err := sp.Commit(ctx); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE import_rows SET transaction_id=$3 WHERE batch_id=$1 AND row_no=$2`, id, r.no, txID); err != nil {
				return err
			}
			created++
		}
		if _, err := tx.Exec(ctx, `UPDATE import_batches SET status='confirmed', confirmed_at=now(), created_count=$2,
			valid_rows=(SELECT count(*) FROM import_rows WHERE batch_id=$1 AND status='valid'),
			invalid_rows=(SELECT count(*) FROM import_rows WHERE batch_id=$1 AND status='invalid'),
			duplicate_rows=(SELECT count(*) FROM import_rows WHERE batch_id=$1 AND status='duplicate') WHERE id=$1`, id, created); err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "import.confirmed", map[string]any{"batch_id": id, "created": created}); err != nil {
			return err
		}
		out, err = s.get(ctx, tx, userID, id)
		return err
	})
	return out, err
}

func errorText(err error) string {
	if ae, ok := err.(*apierr.Error); ok {
		if len(ae.Fields) > 0 {
			for _, v := range ae.Fields {
				return v
			}
		}
		return ae.Message
	}
	return "Could not be imported."
}

// Undo voids every transaction the batch created, in one database transaction. History is kept (reversed).
func (s *Service) Undo(ctx context.Context, userID, id string) (Batch, error) {
	var out Batch
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM import_batches WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, userID).Scan(&status)
		if dbx.IsNoRows(err) {
			return apierr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if status == "undone" {
			out, err = s.get(ctx, tx, userID, id)
			return err
		}
		if status != "confirmed" {
			return ErrState
		}
		rows, err := tx.Query(ctx, `SELECT transaction_id::text FROM import_rows WHERE batch_id=$1 AND transaction_id IS NOT NULL ORDER BY row_no`, id)
		if err != nil {
			return err
		}
		var txIDs []string
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return err
			}
			txIDs = append(txIDs, t)
		}
		rows.Close()
		for _, t := range txIDs {
			if err := s.txs.VoidInTx(ctx, tx, userID, t); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE import_batches SET status='undone', undone_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
		if err := audit.Log(ctx, tx, userID, "import.undone", map[string]any{"batch_id": id, "voided": len(txIDs)}); err != nil {
			return err
		}
		out, err = s.get(ctx, tx, userID, id)
		return err
	})
	return out, err
}

// Template is a small example file users can copy.
const Template = "Date,Description,Amount,Reference,Category\n2026-11-18,Swiggy dinner,-482.00,UPI4821991,Food and dining\n2026-11-15,Salary,112400.00,NEFT20261115,Salary\n"

// ---- export ----

type ExportParams struct {
	AccountID string `json:"account_id"`
	From      string `json:"from"`
	To        string `json:"to"`
}

// Export streams the user's transactions as CSV. The file is sensitive financial data: it is only ever returned to
// the authenticated owner, with no-store caching, an audit event, and spreadsheet-formula neutralisation.
func (s *Service) Export(ctx context.Context, userID string, p ExportParams, w io.Writer) (int, error) {
	loc, tzName, err := s.loc(userID)
	if err != nil {
		return 0, err
	}
	var from, to *time.Time
	if p.From != "" {
		d, err := ParseDate(p.From, "ymd")
		if err != nil {
			return 0, apierr.Validation(map[string]string{"from": "Use a date like 2026-11-18."})
		}
		t := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc).UTC()
		from = &t
	}
	if p.To != "" {
		d, err := ParseDate(p.To, "ymd")
		if err != nil {
			return 0, apierr.Validation(map[string]string{"to": "Use a date like 2026-11-18."})
		}
		t := time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, loc).UTC()
		to = &t
	}
	var acct any
	if p.AccountID != "" {
		a := ids.Normalize(p.AccountID)
		if !ids.Valid(a) {
			return 0, apierr.Validation(map[string]string{"account_id": "Choose one of your accounts."})
		}
		if _, err := accounts.Fetch(ctx, s.db, userID, a); err != nil {
			return 0, apierr.ErrNotFound
		}
		acct = a
	}
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"Date", "Time", "Account", "Type", "Status", "Amount", "Currency", "Title", "Category", "Payment method", "Payee", "Notes", "Tags", "Reference", "Source"})
	n := 0
	var lastAt *time.Time
	var lastID *string
	for {
		rows, err := s.db.Query(ctx, `SELECT t.id::text, t.occurred_at, to_char(t.occurred_at AT TIME ZONE $6,'YYYY-MM-DD'), to_char(t.occurred_at AT TIME ZONE $6,'HH24:MI'),
			a.name, t.type, t.transfer_direction, t.status, t.amount, t.currency, t.title, COALESCE(c.name,''), t.payment_method, t.payee_name, t.notes,
			COALESCE((SELECT string_agg(g.name, ', ' ORDER BY lower(g.name)) FROM transaction_tags x JOIN tags g ON g.id = x.tag_id WHERE x.transaction_id = t.id),''), t.external_ref, t.source
			FROM transactions t JOIN accounts a ON a.id = t.account_id LEFT JOIN categories c ON c.id = t.category_id
			WHERE t.user_id=$1 AND t.deleted_at IS NULL AND ($2::uuid IS NULL OR t.account_id=$2) AND ($3::timestamptz IS NULL OR t.occurred_at >= $3) AND ($4::timestamptz IS NULL OR t.occurred_at < $4)
			  AND ($5::timestamptz IS NULL OR (t.occurred_at, t.id) < ($5, $7::uuid))
			ORDER BY t.occurred_at DESC, t.id DESC LIMIT 500`, userID, acct, from, to, lastAt, tzName, lastID)
		if err != nil {
			return n, err
		}
		count := 0
		for rows.Next() {
			var id, d, tm, acctName, typ, status, cur, title, cat, pm, payee, notes, tags, ref, src string
			var at time.Time
			var dir *string
			var amt int64
			if err := rows.Scan(&id, &at, &d, &tm, &acctName, &typ, &dir, &status, &amt, &cur, &title, &cat, &pm, &payee, &notes, &tags, &ref, &src); err != nil {
				rows.Close()
				return n, err
			}
			c, _ := money.Lookup(cur)
			signed := amt
			if typ == "expense" || (typ == "transfer" && dir != nil && *dir == "out") {
				signed = -amt
			}
			label := typ
			if typ == "transfer" && dir != nil {
				label = "transfer " + *dir
			}
			_ = cw.Write([]string{d, tm, SafeCell(acctName), label, status, plain(signed, c.Exponent), cur, SafeCell(title), SafeCell(cat), pm, SafeCell(payee), SafeCell(notes), SafeCell(tags), SafeCell(ref), src})
			lastAt, lastID = &at, &id
			count++
			n++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return n, err
		}
		if count < 500 {
			break
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return n, err
	}
	_ = audit.Log(ctx, s.db, userID, "export.created", map[string]any{"rows": n})
	return n, nil
}

// plain renders minor units as a decimal string ("-1250.50") for files meant to be read by people and spreadsheets.
func plain(minor int64, exp int) string {
	neg := minor < 0
	u := uint64(minor)
	if neg {
		u = uint64(-(minor + 1)) + 1
	}
	div := uint64(1)
	for i := 0; i < exp; i++ {
		div *= 10
	}
	s := strconv.FormatUint(u/div, 10)
	if exp > 0 {
		f := strconv.FormatUint(u%div, 10)
		s += "." + strings.Repeat("0", exp-len(f)) + f
	}
	if neg {
		s = "-" + s
	}
	return s
}

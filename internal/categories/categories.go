// Package categories owns categories (built-in and custom, with one level of subcategories) and tags.
// Built-in rows have user_id NULL: visible to everyone, read-only. Custom rows belong to one user.
package categories

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/auth"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/httpx"
	"plinth/server/internal/platform/ids"
)

type Category struct {
	ID       string  `json:"id"`
	ParentID *string `json:"parent_id"`
	Kind     string  `json:"kind"` // expense | income
	Name     string  `json:"name"`
	Icon     string  `json:"icon"`
	System   bool    `json:"is_system"`
	Archived bool    `json:"archived"`
}

type Tag struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var (
	ErrReadOnly = apierr.New(http.StatusForbidden, "category_read_only", "Built-in categories cannot be changed. Create your own category instead.")
	ErrDup      = apierr.New(http.StatusConflict, "category_exists", "You already have a category with that name here.")
)

type Service struct{ db *pgxpool.Pool }

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

const cols = `id::text, parent_id::text, kind, name, icon, user_id IS NULL, archived_at IS NOT NULL`

func scan(row interface{ Scan(...any) error }) (Category, error) {
	var c Category
	err := row.Scan(&c.ID, &c.ParentID, &c.Kind, &c.Name, &c.Icon, &c.System, &c.Archived)
	return c, err
}

// List returns built-in categories plus the user's own. Archived ones are hidden unless includeArchived.
func (s *Service) List(ctx context.Context, userID, kind string, includeArchived bool) ([]Category, error) {
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM categories
		WHERE (user_id IS NULL OR user_id = $1) AND ($2 = '' OR kind = $2) AND ($3 OR archived_at IS NULL)
		ORDER BY kind, parent_id NULLS FIRST, user_id IS NOT NULL, lower(name)`, userID, kind, includeArchived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Category{}
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Visible returns a category the user may use (built-in or their own, not archived).
func Visible(ctx context.Context, q dbx.Q, userID, id string) (Category, error) {
	c, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM categories WHERE id=$1 AND (user_id IS NULL OR user_id=$2) AND archived_at IS NULL`, id, userID))
	if dbx.IsNoRows(err) {
		return c, apierr.Validation(map[string]string{"category_id": "Choose one of your categories."})
	}
	return c, err
}

type CreateInput struct {
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	ParentID *string `json:"parent_id"`
	Icon     string  `json:"icon"`
}

func clean(s string) string { return strings.TrimSpace(s) }

func (s *Service) Create(ctx context.Context, userID string, in CreateInput) (Category, error) {
	f := map[string]string{}
	in.Name = clean(in.Name)
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 60 {
		f["name"] = "Enter a name of 1 to 60 characters."
	}
	if in.Kind != "expense" && in.Kind != "income" {
		f["kind"] = "Choose expense or income."
	}
	if len(in.Icon) > 30 {
		f["icon"] = "Icon name is too long."
	}
	if len(f) > 0 {
		return Category{}, apierr.Validation(f)
	}
	if in.ParentID != nil {
		pid := ids.Normalize(*in.ParentID)
		if !ids.Valid(pid) {
			return Category{}, apierr.Validation(map[string]string{"parent_id": "Choose a valid parent category."})
		}
		p, err := Visible(ctx, s.db, userID, pid)
		if err != nil || p.ParentID != nil || p.Kind != in.Kind {
			return Category{}, apierr.Validation(map[string]string{"parent_id": "Choose a top-level category of the same kind."})
		}
		in.ParentID = &pid
	}
	c, err := scan(s.db.QueryRow(ctx, `INSERT INTO categories (user_id, parent_id, kind, name, icon) VALUES ($1,$2,$3,$4,$5) RETURNING `+cols,
		userID, in.ParentID, in.Kind, in.Name, in.Icon))
	if dbx.PgCode(err) == dbx.UniqueViolation {
		return Category{}, ErrDup
	}
	return c, err
}

type UpdateInput struct {
	Name     *string `json:"name"`
	Icon     *string `json:"icon"`
	Archived *bool   `json:"archived"`
}

func (s *Service) Update(ctx context.Context, userID, id string, in UpdateInput) (Category, error) {
	var system bool
	err := s.db.QueryRow(ctx, `SELECT user_id IS NULL FROM categories WHERE id=$1 AND (user_id IS NULL OR user_id=$2)`, id, userID).Scan(&system)
	if dbx.IsNoRows(err) {
		return Category{}, apierr.ErrNotFound
	}
	if err != nil {
		return Category{}, err
	}
	if system {
		return Category{}, ErrReadOnly
	}
	if in.Name != nil {
		n := clean(*in.Name)
		if c := utf8.RuneCountInString(n); c < 1 || c > 60 {
			return Category{}, apierr.Validation(map[string]string{"name": "Enter a name of 1 to 60 characters."})
		}
		in.Name = &n
	}
	if in.Icon != nil && len(*in.Icon) > 30 {
		return Category{}, apierr.Validation(map[string]string{"icon": "Icon name is too long."})
	}
	var archivedAt *time.Time
	if in.Archived != nil {
		if *in.Archived {
			t := time.Now()
			archivedAt = &t
		}
	}
	c, err := scan(s.db.QueryRow(ctx, `UPDATE categories SET name = COALESCE($3, name), icon = COALESCE($4, icon),
		archived_at = CASE WHEN $5::boolean IS NULL THEN archived_at WHEN $5 THEN COALESCE(archived_at, $6) ELSE NULL END
		WHERE id=$1 AND user_id=$2 RETURNING `+cols, id, userID, in.Name, in.Icon, in.Archived, archivedAt))
	if dbx.PgCode(err) == dbx.UniqueViolation {
		return Category{}, ErrDup
	}
	if err == nil && in.Archived != nil && *in.Archived {
		// Archiving a category also archives its subcategories, so none is left hanging under a hidden parent.
		_, err = s.db.Exec(ctx, `UPDATE categories SET archived_at = COALESCE(archived_at, $3) WHERE parent_id=$1 AND user_id=$2`, id, userID, archivedAt)
	}
	return c, err
}

// Tags lists the user's tags.
func (s *Service) Tags(ctx context.Context, userID string) ([]Tag, error) {
	rows, err := s.db.Query(ctx, `SELECT id::text, name FROM tags WHERE user_id=$1 ORDER BY lower(name)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Tag{}
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/categories", require(http.HandlerFunc(h.list)))
	mux.Handle("POST /v1/categories", require(http.HandlerFunc(h.create)))
	mux.Handle("PUT /v1/categories/{id}", require(http.HandlerFunc(h.update)))
	mux.Handle("GET /v1/tags", require(http.HandlerFunc(h.tags)))
}

func principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, r, apierr.ErrUnauthorized)
	}
	return p, ok
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != "expense" && kind != "income" {
		httpx.Error(w, r, apierr.Validation(map[string]string{"kind": "Choose expense or income."}))
		return
	}
	out, err := h.svc.List(r.Context(), p.UserID, kind, r.URL.Query().Get("include_archived") == "true")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var in CreateInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.Create(r.Context(), p.UserID, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, out)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id := ids.Normalize(r.PathValue("id"))
	if !ids.Valid(id) {
		httpx.Error(w, r, apierr.ErrNotFound)
		return
	}
	var in UpdateInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.Update(r.Context(), p.UserID, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) tags(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Tags(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

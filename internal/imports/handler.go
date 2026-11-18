package imports

import (
	"net/http"
	"strconv"

	"plinth/server/internal/auth"
	"plinth/server/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	r := func(f http.HandlerFunc) http.Handler { return require(f) }
	mux.Handle("GET /v1/imports/template", r(h.template))
	mux.Handle("POST /v1/imports", r(h.upload))
	mux.Handle("GET /v1/imports", r(h.list))
	mux.Handle("GET /v1/imports/{id}", r(h.get))
	mux.Handle("PUT /v1/imports/{id}/mapping", r(h.mapping))
	mux.Handle("GET /v1/imports/{id}/preview", r(h.preview))
	mux.Handle("GET /v1/imports/{id}/errors", r(h.errors))
	mux.Handle("POST /v1/imports/{id}/confirm", r(h.confirm))
	mux.Handle("POST /v1/imports/{id}/undo", r(h.undo))
	mux.Handle("POST /v1/exports", r(h.export))
}

func (h *Handler) template(w http.ResponseWriter, rq *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="plinth-import-template.csv"`)
	_, _ = w.Write([]byte(Template))
}

func (h *Handler) upload(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	var in UploadInput
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	b, err := h.svc.Upload(rq.Context(), uid, in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, b)
}

func (h *Handler) list(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	out, err := h.svc.List(rq.Context(), uid)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) get(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	b, err := h.svc.Get(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, b)
}

func (h *Handler) mapping(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	var m Mapping
	if err := httpx.Decode(rq, &m); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	b, err := h.svc.SetMapping(rq.Context(), uid, id, m)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, b)
}

func (h *Handler) rows(w http.ResponseWriter, rq *http.Request, status string) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(rq.URL.Query().Get("limit"))
	if status == "" {
		status = rq.URL.Query().Get("status")
	}
	b, rows, err := h.svc.Rows(rq.Context(), uid, id, status, limit)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"batch": b, "rows": rows})
}

func (h *Handler) preview(w http.ResponseWriter, rq *http.Request) { h.rows(w, rq, "") }
func (h *Handler) errors(w http.ResponseWriter, rq *http.Request)  { h.rows(w, rq, "invalid") }

func (h *Handler) confirm(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	var in struct {
		IncludeDuplicateRows []int `json:"include_duplicate_rows"`
	}
	if rq.ContentLength != 0 {
		if err := httpx.Decode(rq, &in); err != nil {
			httpx.Error(w, rq, err)
			return
		}
	}
	b, err := h.svc.Confirm(rq.Context(), uid, id, in.IncludeDuplicateRows)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, b)
}

func (h *Handler) undo(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	b, err := h.svc.Undo(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, b)
}

func (h *Handler) export(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	var p ExportParams
	if rq.ContentLength != 0 {
		if err := httpx.Decode(rq, &p); err != nil {
			httpx.Error(w, rq, err)
			return
		}
	}
	// Exported files are sensitive financial data (CLAUDE.md 3.13): never cached, only for the signed-in owner.
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="plinth-transactions.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := h.svc.Export(rq.Context(), uid, p, w); err != nil {
		// Headers may already be sent; for a validation error nothing has been written yet.
		w.Header().Del("Content-Disposition")
		httpx.Error(w, rq, err)
	}
}

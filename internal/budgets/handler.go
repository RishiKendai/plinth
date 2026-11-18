package budgets

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
	mux.Handle("POST /v1/budgets", r(h.create))
	mux.Handle("GET /v1/budgets", r(h.list))
	mux.Handle("GET /v1/budgets/{id}", r(h.get))
	mux.Handle("PUT /v1/budgets/{id}", r(h.update))
	mux.Handle("POST /v1/budgets/{id}/archive", r(h.archive(true)))
	mux.Handle("POST /v1/budgets/{id}/unarchive", r(h.archive(false)))
	mux.Handle("GET /v1/budgets/{id}/history", r(h.history))
}

func (h *Handler) create(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	var in Input
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	out, err := h.svc.Create(rq.Context(), uid, rq.Header.Get(httpx.IdempotencyHeader), in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, out)
}

func (h *Handler) list(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	out, err := h.svc.List(rq.Context(), uid, rq.URL.Query().Get("include_archived") == "true")
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
	out, err := h.svc.Get(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) update(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	var in UpdateInput
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	out, err := h.svc.Update(rq.Context(), uid, id, in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) archive(archive bool) http.HandlerFunc {
	return func(w http.ResponseWriter, rq *http.Request) {
		uid, ok := auth.UserID(w, rq)
		if !ok {
			return
		}
		id, ok := httpx.PathUUID(w, rq, "id")
		if !ok {
			return
		}
		out, err := h.svc.SetArchived(rq.Context(), uid, id, archive)
		if err != nil {
			httpx.Error(w, rq, err)
			return
		}
		httpx.JSON(w, http.StatusOK, out)
	}
}

func (h *Handler) history(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	n, _ := strconv.Atoi(rq.URL.Query().Get("periods"))
	out, err := h.svc.History(rq.Context(), uid, id, n)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

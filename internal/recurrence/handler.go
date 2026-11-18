package recurrence

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
	mux.Handle("POST /v1/recurrences", r(h.create))
	mux.Handle("GET /v1/recurrences", r(h.list))
	mux.Handle("GET /v1/recurrences/{id}", r(h.get))
	mux.Handle("PUT /v1/recurrences/{id}", r(h.update))
	mux.Handle("POST /v1/recurrences/{id}/pause", r(h.status("pause")))
	mux.Handle("POST /v1/recurrences/{id}/resume", r(h.status("resume")))
	mux.Handle("POST /v1/recurrences/{id}/cancel", r(h.status("cancel")))
	mux.Handle("GET /v1/recurrences/{id}/occurrences", r(h.occurrences))
	mux.Handle("POST /v1/recurrences/{id}/occurrences/{oid}/confirm", r(h.confirm))
	mux.Handle("POST /v1/recurrences/{id}/occurrences/{oid}/skip", r(h.skip))
	mux.Handle("GET /v1/occurrences", r(h.allOccurrences))
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
	out, err := h.svc.List(rq.Context(), uid, rq.URL.Query().Get("status"))
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

func (h *Handler) status(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, rq *http.Request) {
		uid, ok := auth.UserID(w, rq)
		if !ok {
			return
		}
		id, ok := httpx.PathUUID(w, rq, "id")
		if !ok {
			return
		}
		out, err := h.svc.SetStatus(rq.Context(), uid, id, action)
		if err != nil {
			httpx.Error(w, rq, err)
			return
		}
		httpx.JSON(w, http.StatusOK, out)
	}
}

func (h *Handler) occurrences(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(rq.URL.Query().Get("limit"))
	out, err := h.svc.ListOccurrences(rq.Context(), uid, id, rq.URL.Query().Get("state"), limit)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) allOccurrences(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	days, _ := strconv.Atoi(rq.URL.Query().Get("days"))
	out, err := h.svc.Occurrences(rq.Context(), uid, rq.URL.Query().Get("state"), days)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) confirm(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	oid, ok := httpx.PathUUID(w, rq, "oid")
	if !ok {
		return
	}
	var in ConfirmInput
	if rq.ContentLength != 0 {
		if err := httpx.Decode(rq, &in); err != nil {
			httpx.Error(w, rq, err)
			return
		}
	}
	out, err := h.svc.Confirm(rq.Context(), uid, rq.Header.Get(httpx.IdempotencyHeader), id, oid, in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) skip(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	oid, ok := httpx.PathUUID(w, rq, "oid")
	if !ok {
		return
	}
	out, err := h.svc.Skip(rq.Context(), uid, id, oid)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

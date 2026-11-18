package upi

import (
	"net/http"

	"plinth/server/internal/auth"
	"plinth/server/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	r := func(f http.HandlerFunc) http.Handler { return require(f) }
	mux.Handle("POST /v1/upi/payments", r(h.start))
	mux.Handle("GET /v1/upi/payments", r(h.list))
	mux.Handle("GET /v1/upi/payments/{id}", r(h.get))
	mux.Handle("POST /v1/upi/payments/{id}/handoff", r(h.handoff))
	mux.Handle("POST /v1/upi/payments/{id}/outcome", r(h.outcome))
}

func (h *Handler) start(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	var in StartInput
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	p, err := h.svc.Start(rq.Context(), uid, in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, p)
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
	p, err := h.svc.Get(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}

func (h *Handler) handoff(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	p, err := h.svc.Handoff(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}

func (h *Handler) outcome(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	var in struct {
		Outcome string `json:"outcome"`
	}
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	p, err := h.svc.Outcome(rq.Context(), uid, rq.Header.Get(httpx.IdempotencyHeader), id, in.Outcome)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}

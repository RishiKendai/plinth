package bankmsg

import (
	"net/http"

	"plinth/server/internal/auth"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	r := func(f http.HandlerFunc) http.Handler { return require(f) }
	mux.Handle("POST /v1/sms-events", r(h.ingest))
	mux.Handle("GET /v1/sms-events", r(h.list))
	mux.Handle("POST /v1/sms-events/{id}/add", r(h.add))
	mux.Handle("POST /v1/sms-events/{id}/link", r(h.link))
	mux.Handle("POST /v1/sms-events/{id}/dismiss", r(h.dismiss))
}

func (h *Handler) ingest(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	var in IngestInput
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	e, err := h.svc.Ingest(rq.Context(), uid, in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, e)
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

func (h *Handler) add(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	var in AddInput
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	e, err := h.svc.Add(rq.Context(), uid, rq.Header.Get(httpx.IdempotencyHeader), id, in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, e)
}

func (h *Handler) link(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	var in struct {
		TransactionID string `json:"transaction_id"`
	}
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	e, err := h.svc.Link(rq.Context(), uid, id, in.TransactionID)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, e)
}

func (h *Handler) dismiss(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	e, err := h.svc.Dismiss(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, e)
}

var _ = apierr.ErrNotFound

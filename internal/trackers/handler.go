package trackers

import (
	"net/http"
	"strconv"

	"plinth/server/internal/auth"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/httpx"
	"plinth/server/internal/platform/ids"
	"plinth/server/internal/transactions"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	r := func(f http.HandlerFunc) http.Handler { return require(f) }
	mux.Handle("POST /v1/trackers", r(h.create))
	mux.Handle("GET /v1/trackers", r(h.list))
	mux.Handle("GET /v1/trackers/{id}", r(h.get))
	mux.Handle("PUT /v1/trackers/{id}", r(h.update))
	mux.Handle("POST /v1/trackers/{id}/archive", r(h.archive(true)))
	mux.Handle("POST /v1/trackers/{id}/unarchive", r(h.archive(false)))
	mux.Handle("GET /v1/trackers/{id}/transactions", r(h.transactions))
	mux.Handle("POST /v1/trackers/{id}/transactions", r(h.add))
	mux.Handle("DELETE /v1/trackers/{id}/transactions/{txid}", r(h.remove))
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

func (h *Handler) add(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	var in struct {
		TransactionIDs []string `json:"transaction_ids"`
	}
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	out, err := h.svc.AddTransactions(rq.Context(), uid, id, in.TransactionIDs)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) remove(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	txid := ids.Normalize(rq.PathValue("txid"))
	if !ids.Valid(txid) {
		httpx.Error(w, rq, apierr.ErrNotFound)
		return
	}
	out, err := h.svc.RemoveTransaction(rq.Context(), uid, id, txid)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) transactions(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	q := rq.URL.Query()
	p := transactions.ListParams{Cursor: q.Get("cursor"), Sort: q.Get("sort"), Asc: q.Get("order") == "asc"}
	p.Limit, _ = strconv.Atoi(q.Get("limit"))
	page, err := h.svc.Transactions(rq.Context(), uid, id, p)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSONPage(w, page.Items, page.NextCursor)
}

package transactions

import (
	"net/http"
	"strconv"

	"plinth/server/internal/auth"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/httpx"
	"plinth/server/internal/platform/ids"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	r := func(f http.HandlerFunc) http.Handler { return require(f) }
	mux.Handle("POST /v1/transactions", r(h.create))
	mux.Handle("GET /v1/transactions", r(h.list))
	mux.Handle("GET /v1/transactions/search", r(h.search))
	mux.Handle("GET /v1/transactions/{id}", r(h.get))
	mux.Handle("PUT /v1/transactions/{id}", r(h.update))
	mux.Handle("POST /v1/transactions/{id}/void", r(h.void))
	mux.Handle("POST /v1/transactions/{id}/status", r(h.status))
	mux.Handle("POST /v1/transactions/{id}/undo", r(h.undo))
	mux.Handle("DELETE /v1/transactions/{id}", r(h.remove))
	mux.Handle("GET /v1/accounts/{id}/transactions", r(h.accountList))
	mux.Handle("POST /v1/accounts/{id}/reconcile", r(h.reconcile))
	mux.Handle("POST /v1/transfers", r(h.createTransfer))
	mux.Handle("GET /v1/transfers/{id}", r(h.getTransfer))
	mux.Handle("POST /v1/transfers/{id}/void", r(h.voidTransfer))
}

func who(w http.ResponseWriter, rq *http.Request) (string, bool) {
	p, ok := auth.FromContext(rq.Context())
	if !ok {
		httpx.Error(w, rq, apierr.ErrUnauthorized)
		return "", false
	}
	return p.UserID, true
}

func pathID(w http.ResponseWriter, rq *http.Request) (string, bool) {
	id := ids.Normalize(rq.PathValue("id"))
	if !ids.Valid(id) {
		httpx.Error(w, rq, apierr.ErrNotFound)
		return "", false
	}
	return id, true
}

func key(rq *http.Request) string { return rq.Header.Get(httpx.IdempotencyHeader) }

func (h *Handler) create(w http.ResponseWriter, rq *http.Request) {
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	var in CreateInput
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	out, err := h.svc.Create(rq.Context(), uid, key(rq), in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, out)
}

func (h *Handler) get(w http.ResponseWriter, rq *http.Request) {
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	id, ok := pathID(w, rq)
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
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	id, ok := pathID(w, rq)
	if !ok {
		return
	}
	var in UpdateInput
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	out, err := h.svc.Update(rq.Context(), uid, key(rq), id, in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) void(w http.ResponseWriter, rq *http.Request) {
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	id, ok := pathID(w, rq)
	if !ok {
		return
	}
	out, err := h.svc.Void(rq.Context(), uid, key(rq), id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) status(w http.ResponseWriter, rq *http.Request) {
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	id, ok := pathID(w, rq)
	if !ok {
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	out, err := h.svc.ChangeStatus(rq.Context(), uid, key(rq), id, in.Status)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func parseList(rq *http.Request) (ListParams, error) {
	q := rq.URL.Query()
	p := ListParams{
		AccountID: ids.Normalize(q.Get("account_id")), CategoryID: ids.Normalize(q.Get("category_id")), Type: q.Get("type"),
		Status: q.Get("status"), PaymentMethod: q.Get("payment_method"), Tag: q.Get("tag"), Q: q.Get("q"),
		From: q.Get("from"), To: q.Get("to"), Sort: q.Get("sort"), Asc: q.Get("order") == "asc", Cursor: q.Get("cursor"),
	}
	f := map[string]string{}
	num := func(name string) *int64 {
		v := q.Get(name)
		if v == "" {
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			f[name] = "Enter whole minor units, for example 10050 for ₹100.50."
			return nil
		}
		return &n
	}
	p.MinAmount, p.MaxAmount = num("min_amount"), num("max_amount")
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			f["limit"] = "Use a number from 1 to 200."
		}
		p.Limit = n
	}
	if o := q.Get("order"); o != "" && o != "asc" && o != "desc" {
		f["order"] = "Use asc or desc."
	}
	if len(f) > 0 {
		return p, apierr.Validation(f)
	}
	return p, nil
}

func (h *Handler) writeList(w http.ResponseWriter, rq *http.Request, p ListParams, uid string) {
	page, err := h.svc.List(rq.Context(), uid, p)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSONPage(w, page.Items, page.NextCursor)
}

func (h *Handler) list(w http.ResponseWriter, rq *http.Request) {
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	p, err := parseList(rq)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	h.writeList(w, rq, p, uid)
}

func (h *Handler) search(w http.ResponseWriter, rq *http.Request) {
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	p, err := parseList(rq)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	if len(p.Q) == 0 {
		httpx.Error(w, rq, apierr.Validation(map[string]string{"q": "Enter something to search for."}))
		return
	}
	h.writeList(w, rq, p, uid)
}

func (h *Handler) accountList(w http.ResponseWriter, rq *http.Request) {
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	id, ok := pathID(w, rq)
	if !ok {
		return
	}
	p, err := parseList(rq)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	p.AccountID = id
	if err := h.svc.ownsAccount(rq.Context(), uid, id); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	h.writeList(w, rq, p, uid)
}

func (h *Handler) reconcile(w http.ResponseWriter, rq *http.Request) {
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	id, ok := pathID(w, rq)
	if !ok {
		return
	}
	var in struct {
		ActualBalance *int64 `json:"actual_balance"`
	}
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	if in.ActualBalance == nil {
		httpx.Error(w, rq, apierr.Validation(map[string]string{"actual_balance": "Enter the balance your bank shows, in minor units."}))
		return
	}
	out, err := h.svc.Reconcile(rq.Context(), uid, key(rq), id, *in.ActualBalance)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) createTransfer(w http.ResponseWriter, rq *http.Request) {
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	var in TransferInput
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	out, err := h.svc.CreateTransfer(rq.Context(), uid, key(rq), in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, out)
}

func (h *Handler) getTransfer(w http.ResponseWriter, rq *http.Request) {
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	id, ok := pathID(w, rq)
	if !ok {
		return
	}
	out, err := h.svc.GetTransfer(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) voidTransfer(w http.ResponseWriter, rq *http.Request) {
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	id, ok := pathID(w, rq)
	if !ok {
		return
	}
	out, err := h.svc.VoidTransfer(rq.Context(), uid, key(rq), id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// undo restores a voided transaction.
func (h *Handler) undo(w http.ResponseWriter, rq *http.Request) {
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	id, ok := pathID(w, rq)
	if !ok {
		return
	}
	out, err := h.svc.Undo(rq.Context(), uid, key(rq), id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// remove deletes (hides) a voided or failed transaction. The ledger keeps its entries.
func (h *Handler) remove(w http.ResponseWriter, rq *http.Request) {
	uid, ok := who(w, rq)
	if !ok {
		return
	}
	id, ok := pathID(w, rq)
	if !ok {
		return
	}
	if err := h.svc.Delete(rq.Context(), uid, key(rq), id); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.NoContent(w)
}

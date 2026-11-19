package groups

import (
	"net/http"

	"plinth/server/internal/auth"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler, limit func(http.Handler) http.Handler) {
	r := func(f http.HandlerFunc) http.Handler { return require(f) }
	mux.Handle("POST /v1/groups", r(h.create))
	mux.Handle("GET /v1/groups", r(h.list))
	mux.Handle("GET /v1/groups/{id}", r(h.get))
	mux.Handle("PUT /v1/groups/{id}", r(h.rename))
	mux.Handle("POST /v1/groups/{id}/archive", r(h.archive(true)))
	mux.Handle("POST /v1/groups/{id}/unarchive", r(h.archive(false)))
	mux.Handle("GET /v1/groups/{id}/balances", r(h.balances))
	mux.Handle("GET /v1/groups/{id}/analytics", r(h.analytics))
	mux.Handle("POST /v1/groups/{id}/members", r(h.addMember))
	mux.Handle("DELETE /v1/groups/{id}/members/{mid}", r(h.removeMember))
	mux.Handle("PUT /v1/groups/{id}/members/{mid}/role", r(h.setRole))
	mux.Handle("POST /v1/groups/{id}/members/{mid}/invite", r(h.invite))
	mux.Handle("POST /v1/group-invites/redeem", limit(r(h.redeem))) // brute-force guarded like sign-in
	mux.Handle("POST /v1/groups/{id}/expenses", r(h.addExpense))
	mux.Handle("GET /v1/groups/{id}/expenses", r(h.listExpenses))
	mux.Handle("GET /v1/group-expenses/{id}", r(h.getExpense))
	mux.Handle("POST /v1/group-expenses/{id}/void", r(h.voidExpense))
	mux.Handle("POST /v1/groups/{id}/settlements", r(h.addSettlement))
	mux.Handle("GET /v1/groups/{id}/settlements", r(h.listSettlements))
	mux.Handle("POST /v1/group-settlements/{id}/void", r(h.voidSettlement))
	mux.Handle("POST /v1/splits/preview", r(h.preview))
}

func (h *Handler) create(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	var in CreateInput
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	g, err := h.svc.Create(rq.Context(), uid, rq.Header.Get(httpx.IdempotencyHeader), in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, g)
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
	g, err := h.svc.Get(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, g)
}

func (h *Handler) rename(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	var in struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
	}
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	g, err := h.svc.Rename(rq.Context(), uid, id, in.Name, in.Version)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, g)
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
		g, err := h.svc.SetArchived(rq.Context(), uid, id, archive)
		if err != nil {
			httpx.Error(w, rq, err)
			return
		}
		httpx.JSON(w, http.StatusOK, g)
	}
}

func (h *Handler) balances(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	b, err := h.svc.Balances(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, b)
}

func (h *Handler) addMember(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	m, err := h.svc.AddMember(rq.Context(), uid, id, in.Name)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, m)
}

func (h *Handler) removeMember(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	mid, ok := httpx.PathUUID(w, rq, "mid")
	if !ok {
		return
	}
	if err := h.svc.RemoveMember(rq.Context(), uid, id, mid); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) setRole(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	mid, ok := httpx.PathUUID(w, rq, "mid")
	if !ok {
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	if err := h.svc.SetRole(rq.Context(), uid, id, mid, in.Role); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) invite(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	mid, ok := httpx.PathUUID(w, rq, "mid")
	if !ok {
		return
	}
	code, exp, err := h.svc.Invite(rq.Context(), uid, id, mid)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"code": code, "expires_at": exp})
}

func (h *Handler) redeem(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	g, err := h.svc.Redeem(rq.Context(), uid, in.Code)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, g)
}

func (h *Handler) addExpense(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	var in ExpenseInput
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	e, err := h.svc.AddExpense(rq.Context(), uid, rq.Header.Get(httpx.IdempotencyHeader), id, in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, e)
}

func (h *Handler) listExpenses(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	out, err := h.svc.ListExpenses(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) getExpense(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	e, err := h.svc.GetExpense(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, e)
}

func (h *Handler) voidExpense(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	e, err := h.svc.VoidExpense(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, e)
}

func (h *Handler) addSettlement(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	var in SettlementInput
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	x, err := h.svc.AddSettlement(rq.Context(), uid, rq.Header.Get(httpx.IdempotencyHeader), id, in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, x)
}

func (h *Handler) listSettlements(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	out, err := h.svc.ListSettlements(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) voidSettlement(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	x, err := h.svc.VoidSettlement(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, x)
}

// preview runs the split engine without saving anything, so the app can show exact shares as the user types.
func (h *Handler) preview(w http.ResponseWriter, rq *http.Request) {
	if _, ok := auth.UserID(w, rq); !ok {
		return
	}
	var in struct {
		Method       string             `json:"split_method"`
		Amount       int64              `json:"amount"`
		Participants []ParticipantInput `json:"participants"`
	}
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	parts := make([]Part, len(in.Participants))
	for i, p := range in.Participants {
		parts[i] = Part{Member: p.MemberID, Value: p.Value}
	}
	shares, err := Compute(in.Method, in.Amount, parts)
	if err != nil {
		httpx.Error(w, rq, apierr.Validation(map[string]string{"participants": capital(err.Error()) + "."}))
		return
	}
	out := make([]ShareView, len(shares))
	for i, s := range shares {
		out[i] = ShareView{MemberID: s.Member, Amount: s.Amount, Input: s.Input}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"shares": out})
}

func (h *Handler) analytics(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	a, err := h.svc.Analytics(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, a)
}

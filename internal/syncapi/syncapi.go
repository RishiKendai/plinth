// Package syncapi serves the incremental pull side of offline sync. Pushing needs no new endpoint: queued writes replay
// through the normal endpoints with the Idempotency-Key they were created with. See docs/sync.md.
package syncapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"plinth/server/internal/accounts"
	"plinth/server/internal/auth"
	"plinth/server/internal/platform/httpx"
	"plinth/server/internal/transactions"
)

type Handler struct {
	acc *accounts.Service
	txs *transactions.Service
}

func NewHandler(a *accounts.Service, t *transactions.Service) *Handler { return &Handler{a, t} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/sync/accounts", require(http.HandlerFunc(h.accounts)))
	mux.Handle("GET /v1/sync/transactions", require(http.HandlerFunc(h.transactions)))
}

type page struct {
	Data       any    `json:"data"`
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
	ServerTime string `json:"server_time"`
}

func limit(rq *http.Request) int { n, _ := strconv.Atoi(rq.URL.Query().Get("limit")); return n }

func (h *Handler) accounts(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	items, next, more, err := h.acc.Changes(rq.Context(), uid, rq.URL.Query().Get("cursor"), limit(rq), transactions.SyncSettle)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	write(w, items, next, more)
}

func (h *Handler) transactions(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	items, next, more, err := h.txs.Changes(rq.Context(), uid, rq.URL.Query().Get("cursor"), limit(rq))
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	write(w, items, next, more)
}

func write(w http.ResponseWriter, items any, next string, more bool) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(page{Data: items, NextCursor: next, HasMore: more, ServerTime: time.Now().UTC().Format(time.RFC3339)})
}

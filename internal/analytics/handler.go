package analytics

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"plinth/server/internal/auth"
	"plinth/server/internal/platform/httpx"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	r := func(f http.HandlerFunc) http.Handler { return require(f) }
	mux.Handle("GET /v1/analytics/overview", r(h.overview))
	mux.Handle("GET /v1/analytics/income", r(h.breakdown("income")))
	mux.Handle("GET /v1/analytics/expenses", r(h.breakdown("expense")))
	mux.Handle("GET /v1/analytics/categories", r(h.breakdown("")))
	mux.Handle("GET /v1/analytics/accounts", r(h.accounts))
	mux.Handle("GET /v1/analytics/cash-flow", r(h.trends))
	mux.Handle("GET /v1/analytics/trends", r(h.trends))
	mux.Handle("GET /v1/analytics/compare", r(h.compare))
	mux.Handle("GET /v1/analytics/heatmap", r(h.heatmap))
	mux.Handle("GET /v1/accounts/{id}/analytics", r(h.account))
	mux.Handle("GET /v1/insights", r(h.insights))
	mux.Handle("POST /v1/reports", r(h.createReport))
	mux.Handle("GET /v1/reports", r(h.listReports))
	mux.Handle("GET /v1/reports/{id}", r(h.getReport))
	mux.Handle("GET /v1/reports/{id}/download", r(h.download))
	mux.Handle("DELETE /v1/reports/{id}", r(h.deleteReport))
}

func (h *Handler) scope(w http.ResponseWriter, rq *http.Request) (Scope, bool) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return Scope{}, false
	}
	q := rq.URL.Query()
	sc, err := h.svc.Resolve(rq.Context(), uid, q.Get("from"), q.Get("to"), q.Get("currency"), q.Get("account_id"))
	if err != nil {
		httpx.Error(w, rq, err)
		return Scope{}, false
	}
	return sc, true
}

func (h *Handler) overview(w http.ResponseWriter, rq *http.Request) {
	sc, ok := h.scope(w, rq)
	if !ok {
		return
	}
	o, err := h.svc.Overview(rq.Context(), sc)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, o)
}

func (h *Handler) breakdown(fixed string) http.HandlerFunc {
	return func(w http.ResponseWriter, rq *http.Request) {
		sc, ok := h.scope(w, rq)
		if !ok {
			return
		}
		q := rq.URL.Query()
		typ := fixed
		if typ == "" {
			typ = q.Get("type")
			if typ == "" {
				typ = "expense"
			}
		}
		by := q.Get("by")
		if by == "" {
			by = "category"
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		rows, total, err := h.svc.Breakdown(rq.Context(), sc, by, typ, limit)
		if err != nil {
			httpx.Error(w, rq, err)
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"from": sc.FromDate, "to": sc.ToDate, "currency": sc.Currency, "by": by, "type": typ, "total": total, "rows": rows})
	}
}

func (h *Handler) accounts(w http.ResponseWriter, rq *http.Request) {
	q := rq.URL.Query()
	q.Set("by", "account")
	rq.URL.RawQuery = q.Encode()
	h.breakdown("")(w, rq)
}

func (h *Handler) trends(w http.ResponseWriter, rq *http.Request) {
	sc, ok := h.scope(w, rq)
	if !ok {
		return
	}
	pts, err := h.svc.Trend(rq.Context(), sc, rq.URL.Query().Get("granularity"))
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"from": sc.FromDate, "to": sc.ToDate, "currency": sc.Currency, "points": pts})
}

func (h *Handler) compare(w http.ResponseWriter, rq *http.Request) {
	sc, ok := h.scope(w, rq)
	if !ok {
		return
	}
	rows, pf, pt, err := h.svc.Compare(rq.Context(), sc)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"from": sc.FromDate, "to": sc.ToDate, "previous_from": pf, "previous_to": pt, "currency": sc.Currency, "rows": rows})
}

func (h *Handler) heatmap(w http.ResponseWriter, rq *http.Request) {
	sc, ok := h.scope(w, rq)
	if !ok {
		return
	}
	ht, err := h.svc.Heatmap(rq.Context(), sc)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, ht)
}

func (h *Handler) account(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	q := rq.URL.Query()
	a, err := h.svc.Account(rq.Context(), uid, id, q.Get("from"), q.Get("to"))
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, a)
}

func (h *Handler) insights(w http.ResponseWriter, rq *http.Request) {
	sc, ok := h.scope(w, rq)
	if !ok {
		return
	}
	in, err := h.svc.Insights(rq.Context(), sc)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"from": sc.FromDate, "to": sc.ToDate, "insights": in})
}

func (h *Handler) createReport(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	var in ReportInput
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	rep, err := h.svc.CreateReport(rq.Context(), uid, rq.Header.Get(httpx.IdempotencyHeader), in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	// Generate in the background right away so the user does not wait for the worker's next sweep. The worker covers
	// anything this misses (process restart). Processing is idempotent.
	if rep.Status == "queued" {
		go func(id string) {
			if err := h.svc.Process(contextBackground(), id); err != nil {
				h.log.Warn("report generation failed", "report_id", id, "err", err.Error())
			}
		}(rep.ID)
	}
	httpx.JSON(w, http.StatusAccepted, rep)
}

func (h *Handler) listReports(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	l, err := h.svc.ListReports(rq.Context(), uid)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, l)
}

func (h *Handler) getReport(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	r, err := h.svc.GetReport(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, r)
}

func (h *Handler) download(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	content, name, err := h.svc.Content(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(content))
}

func (h *Handler) deleteReport(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	if err := h.svc.DeleteReport(rq.Context(), uid, id); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.NoContent(w)
}

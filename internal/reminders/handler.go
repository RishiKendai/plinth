package reminders

import (
	"net/http"
	"time"

	"plinth/server/internal/auth"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	r := func(f http.HandlerFunc) http.Handler { return require(f) }
	mux.Handle("POST /v1/reminders", r(h.create))
	mux.Handle("GET /v1/reminders", r(h.list))
	mux.Handle("GET /v1/reminders/{id}", r(h.get))
	mux.Handle("PUT /v1/reminders/{id}", r(h.update))
	mux.Handle("DELETE /v1/reminders/{id}", r(h.delete))
	mux.Handle("POST /v1/reminders/{id}/snooze", r(h.snooze))
	mux.Handle("POST /v1/reminders/{id}/pay", r(h.pay))
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
	out, err := h.svc.List(rq.Context(), uid, rq.URL.Query().Get("state"))
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

func (h *Handler) delete(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	if err := h.svc.Delete(rq.Context(), uid, id); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) snooze(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	var in struct {
		Until *time.Time `json:"until"`
	}
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	if in.Until == nil {
		httpx.Error(w, rq, apierr.Validation(map[string]string{"until": "Choose when to be reminded again."}))
		return
	}
	out, err := h.svc.Snooze(rq.Context(), uid, id, *in.Until)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) pay(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	var in PayInput
	if rq.ContentLength != 0 {
		if err := httpx.Decode(rq, &in); err != nil {
			httpx.Error(w, rq, err)
			return
		}
	}
	out, err := h.svc.Pay(rq.Context(), uid, rq.Header.Get(httpx.IdempotencyHeader), id, in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

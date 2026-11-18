package accounts

import (
	"net/http"

	"plinth/server/internal/auth"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/httpx"
	"plinth/server/internal/platform/ids"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	r := func(f http.HandlerFunc) http.Handler { return require(f) }
	mux.Handle("POST /v1/accounts", r(h.create))
	mux.Handle("GET /v1/accounts", r(h.list))
	mux.Handle("GET /v1/accounts/summary", r(h.summary))
	mux.Handle("GET /v1/accounts/{id}", r(h.get))
	mux.Handle("PUT /v1/accounts/{id}", r(h.update))
	mux.Handle("POST /v1/accounts/{id}/archive", r(h.archive))
	mux.Handle("POST /v1/accounts/{id}/unarchive", r(h.unarchive))
}

// who returns the authenticated user. The owner always comes from the token, never from the request.
func who(w http.ResponseWriter, r *http.Request) (string, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, r, apierr.ErrUnauthorized)
		return "", false
	}
	return p.UserID, true
}

func pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := ids.Normalize(r.PathValue("id"))
	if !ids.Valid(id) {
		httpx.Error(w, r, apierr.ErrNotFound)
		return "", false
	}
	return id, true
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	uid, ok := who(w, r)
	if !ok {
		return
	}
	var in CreateInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.Create(r.Context(), uid, r.Header.Get(httpx.IdempotencyHeader), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, out)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	uid, ok := who(w, r)
	if !ok {
		return
	}
	out, err := h.svc.List(r.Context(), uid, r.URL.Query().Get("include_archived") == "true")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) summary(w http.ResponseWriter, r *http.Request) {
	uid, ok := who(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Summary(r.Context(), uid)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	uid, ok := who(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Get(r.Context(), uid, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	uid, ok := who(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in UpdateInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.Update(r.Context(), uid, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) archive(w http.ResponseWriter, r *http.Request) {
	uid, ok := who(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Archive(r.Context(), uid, r.Header.Get(httpx.IdempotencyHeader), id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) unarchive(w http.ResponseWriter, r *http.Request) {
	uid, ok := who(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Unarchive(r.Context(), uid, r.Header.Get(httpx.IdempotencyHeader), id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

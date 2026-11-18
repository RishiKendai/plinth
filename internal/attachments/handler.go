package attachments

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
	mux.Handle("POST /v1/attachments/upload-url", r(h.uploadURL))
	mux.Handle("POST /v1/attachments/complete", r(h.complete))
	mux.Handle("GET /v1/attachments/{id}/download-url", r(h.downloadURL))
	mux.Handle("DELETE /v1/attachments/{id}", r(h.delete))
	mux.Handle("GET /v1/transactions/{id}/attachments", r(h.forTransaction))
}

func (h *Handler) uploadURL(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	var in UploadInput
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	a, t, err := h.svc.RequestUpload(rq.Context(), uid, in)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"attachment": a, "upload": t})
}

func (h *Handler) complete(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	var in struct {
		AttachmentID string `json:"attachment_id"`
	}
	if err := httpx.Decode(rq, &in); err != nil {
		httpx.Error(w, rq, err)
		return
	}
	id := ids.Normalize(in.AttachmentID)
	if !ids.Valid(id) {
		httpx.Error(w, rq, apierr.ErrNotFound)
		return
	}
	a, err := h.svc.Complete(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, a)
}

func (h *Handler) downloadURL(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	u, exp, err := h.svc.DownloadURL(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"url": u, "expires_at": exp.UTC()})
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

func (h *Handler) forTransaction(w http.ResponseWriter, rq *http.Request) {
	uid, ok := auth.UserID(w, rq)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, rq, "id")
	if !ok {
		return
	}
	out, err := h.svc.ForTransaction(rq.Context(), uid, id)
	if err != nil {
		httpx.Error(w, rq, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

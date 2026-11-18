package auth

import (
	"context"
	"net/http"
	"strings"

	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/httpx"
)

type ctxKey struct{}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Require authenticates the bearer access token and puts the Principal in the request context.
// Ownership always comes from this principal, never from client-supplied IDs.
func (s *Service) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpx.Error(w, r, apierr.ErrUnauthorized)
			return
		}
		p, err := s.Authenticate(r.Context(), strings.TrimSpace(tok))
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpx.Error(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Register mounts the routes. public routes get the rate limiter; protected routes require a session.
func (h *Handler) Register(mux *http.ServeMux, limit func(http.Handler) http.Handler) {
	pub := func(f http.HandlerFunc) http.Handler { return limit(f) }
	mux.Handle("POST /v1/auth/sign-up", pub(h.signUp))
	mux.Handle("POST /v1/auth/sign-in", pub(h.signIn))
	mux.Handle("POST /v1/auth/refresh", pub(h.refresh))

	prot := func(f http.HandlerFunc) http.Handler { return h.svc.Require(f) }
	mux.Handle("POST /v1/auth/sign-out", prot(h.signOut))
	mux.Handle("POST /v1/auth/change-password", limit(prot(h.changePassword)))
	mux.Handle("GET /v1/sessions", prot(h.listSessions))
	mux.Handle("DELETE /v1/sessions/{id}", prot(h.revokeSession))
	mux.Handle("DELETE /v1/sessions", prot(h.revokeOthers))
}

type authBody struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	DeviceName  string `json:"device_name"`
	Platform    string `json:"platform"`
}

type authResponse struct {
	User    UserView `json:"user"`
	Session Tokens   `json:"session"`
}

func (h *Handler) signUp(w http.ResponseWriter, r *http.Request) {
	var b authBody
	if err := httpx.Decode(r, &b); err != nil {
		httpx.Error(w, r, err)
		return
	}
	u, t, err := h.svc.SignUp(r.Context(), b.Email, b.Password, b.DisplayName, Device{b.DeviceName, b.Platform})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, authResponse{u, t})
}

func (h *Handler) signIn(w http.ResponseWriter, r *http.Request) {
	var b authBody
	if err := httpx.Decode(r, &b); err != nil {
		httpx.Error(w, r, err)
		return
	}
	u, t, err := h.svc.SignIn(r.Context(), b.Email, b.Password, Device{b.DeviceName, b.Platform})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, authResponse{u, t})
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var b struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := httpx.Decode(r, &b); err != nil {
		httpx.Error(w, r, err)
		return
	}
	t, err := h.svc.Refresh(r.Context(), b.RefreshToken)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

func (h *Handler) signOut(w http.ResponseWriter, r *http.Request) {
	p, _ := FromContext(r.Context())
	if err := h.svc.SignOut(r.Context(), p); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	var b struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := httpx.Decode(r, &b); err != nil {
		httpx.Error(w, r, err)
		return
	}
	p, _ := FromContext(r.Context())
	if err := h.svc.ChangePassword(r.Context(), p, b.CurrentPassword, b.NewPassword); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	p, _ := FromContext(r.Context())
	out, err := h.svc.ListSessions(r.Context(), p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) revokeSession(w http.ResponseWriter, r *http.Request) {
	p, _ := FromContext(r.Context())
	if err := h.svc.RevokeSession(r.Context(), p, r.PathValue("id")); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) revokeOthers(w http.ResponseWriter, r *http.Request) {
	p, _ := FromContext(r.Context())
	if err := h.svc.RevokeOthers(r.Context(), p); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

// UserID returns the authenticated user, or writes 401 and returns false. Handlers use it so ownership always
// comes from the validated token and never from anything the client sent.
func UserID(w http.ResponseWriter, r *http.Request) (string, bool) {
	p, ok := FromContext(r.Context())
	if !ok {
		httpx.Error(w, r, apierr.ErrUnauthorized)
		return "", false
	}
	return p.UserID, true
}

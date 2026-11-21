// Package users handles the profile and account-deletion request. It reads and writes only profile columns of
// the users table; credentials stay in the auth module (CLAUDE.md section 4.2 ownership rule).
package users

import (
	"context"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/auth"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/httpx"
)

type PasswordVerifier interface {
	VerifyPassword(ctx context.Context, userID, password string) error
}

type Profile struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	DisplayName string     `json:"display_name"`
	CreatedAt   time.Time  `json:"created_at"`
	DeletionAt  *time.Time `json:"deletion_requested_at,omitempty"`
}

type Service struct {
	db       *pgxpool.Pool
	verifier PasswordVerifier
	now      func() time.Time
}

func NewService(db *pgxpool.Pool, v PasswordVerifier) *Service {
	return &Service{db: db, verifier: v, now: time.Now}
}

const sel = `SELECT id, email, display_name, created_at, deletion_requested_at FROM users WHERE id = $1`

func (s *Service) Get(ctx context.Context, userID string) (Profile, error) {
	var p Profile
	err := s.db.QueryRow(ctx, sel, userID).Scan(&p.ID, &p.Email, &p.DisplayName, &p.CreatedAt, &p.DeletionAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, apierr.ErrNotFound
	}
	return p, err
}

func (s *Service) UpdateName(ctx context.Context, userID, name string) (Profile, error) {
	if n := utf8.RuneCountInString(name); n > 100 {
		return Profile{}, apierr.Validation(map[string]string{"display_name": "Use at most 100 characters."})
	}
	if _, err := s.db.Exec(ctx, `UPDATE users SET display_name = $2, updated_at = $3 WHERE id = $1`, userID, name, s.now()); err != nil {
		return Profile{}, err
	}
	return s.Get(ctx, userID)
}

// RequestDeletion records the request after confirming the password. Nothing is erased here: the account is permanently erased by the
// worker once the grace period has passed (Purger, docs/account-deletion.md), and signing in and cancelling stops it until then.
func (s *Service) RequestDeletion(ctx context.Context, userID, password string) (Profile, error) {
	if err := s.verifier.VerifyPassword(ctx, userID, password); err != nil {
		return Profile{}, err
	}
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET deletion_requested_at = COALESCE(deletion_requested_at, $2), updated_at = $2 WHERE id = $1`, userID, s.now()); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Profile{}, err
	}
	return s.Get(ctx, userID)
}

func (s *Service) CancelDeletion(ctx context.Context, userID string) (Profile, error) {
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET deletion_requested_at = NULL, updated_at = $2 WHERE id = $1`, userID, s.now()); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Profile{}, err
	}
	return s.Get(ctx, userID)
}

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler, limit func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/users/me", require(http.HandlerFunc(h.get)))
	mux.Handle("PUT /v1/users/me", require(http.HandlerFunc(h.put)))
	mux.Handle("POST /v1/users/me/deletion-request", limit(require(http.HandlerFunc(h.requestDeletion))))
	mux.Handle("DELETE /v1/users/me/deletion-request", require(http.HandlerFunc(h.cancelDeletion)))
}

func principal(r *http.Request) (auth.Principal, error) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		return p, apierr.ErrUnauthorized
	}
	return p, nil
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err == nil {
		var out Profile
		if out, err = h.svc.Get(r.Context(), p.UserID); err == nil {
			httpx.JSON(w, http.StatusOK, out)
			return
		}
	}
	httpx.Error(w, r, err)
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var b struct {
		DisplayName string `json:"display_name"`
	}
	if err := httpx.Decode(r, &b); err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.UpdateName(r.Context(), p.UserID, b.DisplayName)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) requestDeletion(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var b struct {
		Password string `json:"password"`
	}
	if err := httpx.Decode(r, &b); err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.RequestDeletion(r.Context(), p.UserID, b.Password)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, out)
}

func (h *Handler) cancelDeletion(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.CancelDeletion(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

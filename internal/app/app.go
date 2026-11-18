// Package app wires modules into the HTTP handler.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/accounts"
	"plinth/server/internal/auth"
	"plinth/server/internal/categories"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/config"
	"plinth/server/internal/platform/httpx"
	"plinth/server/internal/platform/security"
	"plinth/server/internal/settings"
	"plinth/server/internal/transactions"
	"plinth/server/internal/users"
)

type Deps struct {
	Config config.Config
	DB     *pgxpool.Pool
	Log    *slog.Logger
	Hasher security.Hasher
}

const maxBody = 1 << 20 // 1 MiB; uploads go direct to R2 via presigned URLs

// New builds the service graph and returns the root handler.
func New(d Deps) (http.Handler, *auth.Service) {
	authSvc := auth.NewService(auth.NewStore(d.DB), d.Hasher,
		auth.Config{Pepper: d.Config.TokenPepper, AccessTTL: d.Config.AccessTokenTTL, RefreshTTL: d.Config.RefreshTokenTTL}, d.Log)

	// 20 requests/minute per IP with a burst of 10 on credential endpoints.
	limit := httpx.NewRateLimiter(20, 10).Middleware(d.Config.TrustProxy)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := d.DB.Ping(ctx); err != nil {
			httpx.JSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database_unavailable"})
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	auth.NewHandler(authSvc).Register(mux, limit)
	users.NewHandler(users.NewService(d.DB, authSvc)).Register(mux, authSvc.Require, limit)
	settingsSvc := settings.NewService(d.DB)
	settings.NewHandler(settingsSvc).Register(mux, authSvc.Require)
	accounts.NewHandler(accounts.NewService(d.DB, settingsSvc)).Register(mux, authSvc.Require)
	categories.NewHandler(categories.NewService(d.DB)).Register(mux, authSvc.Require)
	transactions.NewHandler(transactions.NewService(d.DB, settingsSvc)).Register(mux, authSvc.Require)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, notFound)
	})

	return httpx.Chain(mux,
		httpx.RequestIDMiddleware,
		httpx.Recover,
		httpx.AccessLog(d.Log),
		httpx.SecurityHeaders(d.Config.IsProduction()),
		httpx.Limit(maxBody, d.Config.RequestTimeout),
	), authSvc
}

var notFound = apierr.ErrNotFound

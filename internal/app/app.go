// Package app wires modules into the HTTP handler.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/accounts"
	"plinth/server/internal/analytics"
	"plinth/server/internal/attachments"
	"plinth/server/internal/auth"
	"plinth/server/internal/bankmsg"
	"plinth/server/internal/budgets"
	"plinth/server/internal/categories"
	"plinth/server/internal/gate"
	"plinth/server/internal/groups"
	"plinth/server/internal/imports"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/config"
	"plinth/server/internal/platform/httpx"
	"plinth/server/internal/platform/security"
	"plinth/server/internal/platform/storage"
	"plinth/server/internal/recurrence"
	"plinth/server/internal/reminders"
	"plinth/server/internal/settings"
	"plinth/server/internal/syncapi"
	"plinth/server/internal/trackers"
	"plinth/server/internal/transactions"
	"plinth/server/internal/users"
)

type Deps struct {
	Config config.Config
	DB     *pgxpool.Pool
	Log    *slog.Logger
	Hasher security.Hasher
	// Store is object storage (Cloudflare R2). Nil when R2 is not configured: attachments then answer 503 instead of pretending.
	Store storage.Store
}

const maxBody = 1 << 20 // 1 MiB; uploads go direct to R2 via presigned URLs

// New builds the service graph and returns the root handler.
func New(d Deps) (http.Handler, *auth.Service) {
	authSvc := auth.NewService(auth.NewStore(d.DB), d.Hasher,
		auth.Config{Pepper: d.Config.TokenPepper, AccessTTL: d.Config.AccessTokenTTL, RefreshTTL: auth.SessionLifetime}, d.Log)

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
	accSvc := accounts.NewService(d.DB, settingsSvc)
	accounts.NewHandler(accSvc).Register(mux, authSvc.Require)
	categories.NewHandler(categories.NewService(d.DB)).Register(mux, authSvc.Require)
	txSvc := transactions.NewService(d.DB, settingsSvc)
	transactions.NewHandler(txSvc).Register(mux, authSvc.Require)
	recurrence.NewHandler(recurrence.NewService(d.DB, settingsSvc, txSvc)).Register(mux, authSvc.Require)
	reminders.NewHandler(reminders.NewService(d.DB, settingsSvc, txSvc)).Register(mux, authSvc.Require)
	budgets.NewHandler(budgets.NewService(d.DB, settingsSvc)).Register(mux, authSvc.Require)
	analytics.NewHandler(analytics.NewService(d.DB, settingsSvc), d.Log).Register(mux, authSvc.Require)
	trackers.NewHandler(trackers.NewService(d.DB, settingsSvc, txSvc)).Register(mux, authSvc.Require)

	// Phase 4: capture and automation.
	attachments.NewHandler(attachments.NewService(d.DB, d.Store)).Register(mux, authSvc.Require)
	catSvc := categories.NewService(d.DB)
	imports.NewHandler(imports.NewService(d.DB, settingsSvc, catSvc, txSvc)).Register(mux, authSvc.Require)
	bankmsg.NewHandler(bankmsg.NewService(d.DB, txSvc)).Register(mux, authSvc.Require)

	// Phase 5: groups, splits and settlements.
	groups.NewHandler(groups.NewService(d.DB, settingsSvc, txSvc)).Register(mux, authSvc.Require, limit)

	// Phase 7: incremental sync and service status.
	syncapi.NewHandler(accSvc, txSvc).Register(mux, authSvc.Require)
	gcfg := gate.Config{Maintenance: d.Config.MaintenanceMode, MinClientVersion: d.Config.MinClientVersion, ServerVersion: "0.1.0"}
	mux.HandleFunc("GET /v1/meta", gate.Meta(gcfg))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, notFound)
	})

	return httpx.Chain(mux,
		httpx.RequestIDMiddleware,
		httpx.Recover,
		httpx.AccessLog(d.Log),
		httpx.SecurityHeaders(d.Config.IsProduction()),
		sizeLimit(d.Config.RequestTimeout),
		gate.Middleware(gcfg),
	), authSvc
}

var notFound = apierr.ErrNotFound

// sizeLimit caps request bodies at 1 MiB, except a CSV upload (a few thousand rows of text), which may be larger.
// Receipts never pass through here: they go straight to object storage through presigned URLs.
func sizeLimit(timeout time.Duration) func(http.Handler) http.Handler {
	small := httpx.Limit(maxBody, timeout)
	large := httpx.Limit(imports.MaxContent+(1<<20), timeout)
	return func(next http.Handler) http.Handler {
		s, l := small(next), large(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/v1/imports" {
				l.ServeHTTP(w, r)
				return
			}
			s.ServeHTTP(w, r)
		})
	}
}

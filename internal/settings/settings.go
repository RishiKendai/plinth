// Package settings stores per-user preferences. Currency is an ISO 4217 code; INR is only the default.
package settings

import (
	"context"
	"net/http"
	"time"
	_ "time/tzdata" // IANA zones embedded so validation works in minimal containers

	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/auth"
	"plinth/server/internal/money"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/httpx"
)

type Settings struct {
	Currency             string    `json:"currency"`
	DateFormat           string    `json:"date_format"`   // dmy | mdy | ymd
	NumberFormat         string    `json:"number_format"` // indian | international
	Timezone             string    `json:"timezone"`      // IANA name
	Theme                string    `json:"theme"`         // light | dark | system
	NotificationsEnabled bool      `json:"notifications_enabled"`
	HideAmounts          bool      `json:"hide_amounts"`
	ShowDecimals         bool      `json:"show_decimals"`
	Version              int       `json:"version"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// Patch holds optional changes; nil means unchanged.
type Patch struct {
	Currency             *string `json:"currency"`
	DateFormat           *string `json:"date_format"`
	NumberFormat         *string `json:"number_format"`
	Timezone             *string `json:"timezone"`
	Theme                *string `json:"theme"`
	NotificationsEnabled *bool   `json:"notifications_enabled"`
	HideAmounts          *bool   `json:"hide_amounts"`
	ShowDecimals         *bool   `json:"show_decimals"`
}

type Service struct {
	db  *pgxpool.Pool
	now func() time.Time
}

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db, now: time.Now} }

const cols = `currency, date_format, number_format, timezone, theme, notifications_enabled, hide_amounts, show_decimals, version, updated_at`

func scan(row interface{ Scan(...any) error }) (Settings, error) {
	var s Settings
	err := row.Scan(&s.Currency, &s.DateFormat, &s.NumberFormat, &s.Timezone, &s.Theme, &s.NotificationsEnabled, &s.HideAmounts, &s.ShowDecimals, &s.Version, &s.UpdatedAt)
	return s, err
}

func (s *Service) Get(ctx context.Context, userID string) (Settings, error) {
	if _, err := s.db.Exec(ctx, `INSERT INTO user_settings (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, userID); err != nil {
		return Settings{}, err
	}
	return scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM user_settings WHERE user_id = $1`, userID))
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// Validate returns field errors for the supplied values.
func (p Patch) Validate() map[string]string {
	f := map[string]string{}
	if p.Currency != nil {
		if _, ok := money.Lookup(*p.Currency); !ok {
			f["currency"] = "Choose a supported currency."
		}
	}
	if p.DateFormat != nil && !oneOf(*p.DateFormat, "dmy", "mdy", "ymd") {
		f["date_format"] = "Choose a valid date format."
	}
	if p.NumberFormat != nil && !oneOf(*p.NumberFormat, "indian", "international") {
		f["number_format"] = "Choose a valid number format."
	}
	if p.Theme != nil && !oneOf(*p.Theme, "light", "dark", "system") {
		f["theme"] = "Choose light, dark or system."
	}
	if p.Timezone != nil {
		if _, err := time.LoadLocation(*p.Timezone); err != nil || *p.Timezone == "" || *p.Timezone == "Local" {
			f["timezone"] = "Choose a valid time zone, for example Asia/Kolkata."
		}
	}
	return f
}

func (s *Service) Update(ctx context.Context, userID string, p Patch) (Settings, error) {
	if f := p.Validate(); len(f) > 0 {
		return Settings{}, apierr.Validation(f)
	}
	if _, err := s.Get(ctx, userID); err != nil { // ensures the row exists
		return Settings{}, err
	}
	return scan(s.db.QueryRow(ctx, `
		UPDATE user_settings SET
		  currency = COALESCE($2, currency), date_format = COALESCE($3, date_format),
		  number_format = COALESCE($4, number_format), timezone = COALESCE($5, timezone),
		  theme = COALESCE($6, theme), notifications_enabled = COALESCE($7, notifications_enabled),
		  hide_amounts = COALESCE($8, hide_amounts), show_decimals = COALESCE($9, show_decimals),
		  version = version + 1, updated_at = $10
		WHERE user_id = $1 RETURNING `+cols,
		userID, p.Currency, p.DateFormat, p.NumberFormat, p.Timezone, p.Theme, p.NotificationsEnabled, p.HideAmounts, p.ShowDecimals, s.now()))
}

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/settings", require(http.HandlerFunc(h.get)))
	mux.Handle("PATCH /v1/settings", require(http.HandlerFunc(h.patch)))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, r, apierr.ErrUnauthorized)
		return
	}
	out, err := h.svc.Get(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) patch(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, r, apierr.ErrUnauthorized)
		return
	}
	var body Patch
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.Update(r.Context(), p.UserID, body)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

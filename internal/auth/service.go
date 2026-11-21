// Package auth implements sign-up, sign-in and sessions.
// Access and refresh tokens are opaque random values; only HMAC hashes are stored, so sessions can be revoked
// immediately and a database leak does not expose live tokens.
package auth

import (
	"context"
	"log/slog"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/security"
)

const (
	maxFailedLogins = 5
	lockDuration    = 15 * time.Minute
	MinPasswordLen  = 10
	maxPasswordLen  = 128
)

// SessionLifetime is how long a sign-in lasts: effectively forever. A person stays signed in until they sign out (or change their
// password, which signs out the other devices). The short access token is renewed in the background with the refresh token.
const SessionLifetime = 10 * 365 * 24 * time.Hour

type Config struct {
	Pepper                []byte
	AccessTTL, RefreshTTL time.Duration
}

type Service struct {
	store  *Store
	hasher security.Hasher
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
}

func NewService(store *Store, hasher security.Hasher, cfg Config, log *slog.Logger) *Service {
	return &Service{store: store, hasher: hasher, cfg: cfg, log: log, now: time.Now}
}

type Principal struct{ UserID, SessionID string }

type UserView struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	DisplayName string     `json:"display_name"`
	CreatedAt   time.Time  `json:"created_at"`
	DeletionAt  *time.Time `json:"deletion_requested_at,omitempty"`
}

type Tokens struct {
	SessionID      string    `json:"session_id"`
	AccessToken    string    `json:"access_token"`
	AccessExpires  time.Time `json:"access_expires_at"`
	RefreshToken   string    `json:"refresh_token"`
	RefreshExpires time.Time `json:"refresh_expires_at"`
}

type SessionView struct {
	ID         string    `json:"id"`
	DeviceName string    `json:"device_name"`
	Platform   string    `json:"platform"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	Current    bool      `json:"current"`
}

type Device struct{ Name, Platform string }

func view(u *user) UserView {
	return UserView{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName, CreatedAt: u.CreatedAt, DeletionAt: u.DeletionAt}
}

var (
	errInvalidCreds = apierr.New(http.StatusUnauthorized, "invalid_credentials",
		"Email or password is incorrect, or sign-in is paused for a few minutes after repeated mistakes.")
	errEmailTaken = apierr.New(http.StatusConflict, "email_taken", "An account with this email already exists. Sign in instead.")
)

// ---- validation ----

var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func normalizeEmail(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 254 {
		return "", false
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s {
		return "", false
	}
	at := strings.LastIndex(s, "@")
	return s, at > 0 && domainRe.MatchString(s[at+1:])
}

func checkPassword(pw, email string) string {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < MinPasswordLen:
		return "Use at least 10 characters."
	case n > maxPasswordLen:
		return "Use at most 128 characters."
	case strings.EqualFold(pw, email):
		return "Password must not be your email address."
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n])
	}
	return s
}

func cleanDevice(d Device) Device {
	return Device{Name: clip(strings.Map(stripControl, d.Name), 100), Platform: clip(strings.Map(stripControl, d.Platform), 30)}
}

func stripControl(r rune) rune {
	if r < 32 || r == 127 {
		return -1
	}
	return r
}

// ---- operations ----

func (s *Service) SignUp(ctx context.Context, emailIn, password, displayName string, dev Device) (UserView, Tokens, error) {
	email, ok := normalizeEmail(emailIn)
	fields := map[string]string{}
	if !ok {
		fields["email"] = "Enter a valid email address."
	}
	if msg := checkPassword(password, email); msg != "" {
		fields["password"] = msg
	}
	name := clip(displayName, 100)
	if len(fields) > 0 {
		return UserView{}, Tokens{}, apierr.Validation(fields)
	}
	hash, err := s.hasher.Hash(password)
	if err != nil {
		return UserView{}, Tokens{}, err
	}
	u, err := s.store.createUser(ctx, email, hash, name)
	if err == errEmailTaken0 {
		return UserView{}, Tokens{}, errEmailTaken
	}
	if err != nil {
		return UserView{}, Tokens{}, err
	}
	tok, err := s.issue(ctx, u.ID, cleanDevice(dev))
	if err != nil {
		return UserView{}, Tokens{}, err
	}
	return view(u), tok, nil
}

func (s *Service) SignIn(ctx context.Context, emailIn, password string, dev Device) (UserView, Tokens, error) {
	email, _ := normalizeEmail(emailIn)
	u, err := s.store.userByEmail(ctx, email)
	if err != nil {
		return UserView{}, Tokens{}, err
	}
	now := s.now()
	if u == nil || (u.LockedUntil != nil && u.LockedUntil.After(now)) {
		s.hasher.Dummy(password) // equalise timing whether or not the account exists or is locked
		return UserView{}, Tokens{}, errInvalidCreds
	}
	good, verr := s.hasher.Verify(password, u.PasswordHash)
	if verr != nil || !good {
		if err := s.store.recordFailedLogin(ctx, u.ID, maxFailedLogins, lockDuration, now); err != nil {
			return UserView{}, Tokens{}, err
		}
		return UserView{}, Tokens{}, errInvalidCreds
	}
	if err := s.store.clearFailedLogins(ctx, u.ID); err != nil {
		return UserView{}, Tokens{}, err
	}
	tok, err := s.issue(ctx, u.ID, cleanDevice(dev))
	if err != nil {
		return UserView{}, Tokens{}, err
	}
	return view(u), tok, nil
}

func (s *Service) issue(ctx context.Context, userID string, dev Device) (Tokens, error) {
	access, err := security.NewToken("pat_")
	if err != nil {
		return Tokens{}, err
	}
	refresh, err := security.NewToken("prt_")
	if err != nil {
		return Tokens{}, err
	}
	now := s.now()
	t := Tokens{AccessToken: access, AccessExpires: now.Add(s.cfg.AccessTTL), RefreshToken: refresh, RefreshExpires: now.Add(s.cfg.RefreshTTL)}
	t.SessionID, err = s.store.createSession(ctx, newSession{
		UserID: userID, Device: dev.Name, Platform: dev.Platform,
		AccessHash: security.HashToken(s.cfg.Pepper, access), RefreshHash: security.HashToken(s.cfg.Pepper, refresh),
		AccessExpires: t.AccessExpires, RefreshExpires: t.RefreshExpires, Now: now,
	})
	return t, err
}

func (s *Service) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	if refreshToken == "" {
		return Tokens{}, apierr.ErrUnauthorized
	}
	access, err := security.NewToken("pat_")
	if err != nil {
		return Tokens{}, err
	}
	refresh, err := security.NewToken("prt_")
	if err != nil {
		return Tokens{}, err
	}
	now := s.now()
	t := Tokens{AccessToken: access, AccessExpires: now.Add(s.cfg.AccessTTL), RefreshToken: refresh, RefreshExpires: now.Add(s.cfg.RefreshTTL)}
	_, sid, err := s.store.rotate(ctx, rotation{
		OldRefreshHash: security.HashToken(s.cfg.Pepper, refreshToken),
		NewAccessHash:  security.HashToken(s.cfg.Pepper, access), NewRefreshHash: security.HashToken(s.cfg.Pepper, refresh),
		AccessExpires: t.AccessExpires, RefreshExpires: t.RefreshExpires, Now: now,
	})
	switch err {
	case nil:
		t.SessionID = sid
		return t, nil
	case ErrUnauthenticated, errRefreshReuse:
		return Tokens{}, apierr.New(http.StatusUnauthorized, "session_expired", "Your session has ended. Sign in again.")
	}
	return Tokens{}, err
}

func (s *Service) Authenticate(ctx context.Context, accessToken string) (Principal, error) {
	if accessToken == "" {
		return Principal{}, apierr.ErrUnauthorized
	}
	p, err := s.store.authenticate(ctx, security.HashToken(s.cfg.Pepper, accessToken), s.now())
	if err == ErrUnauthenticated {
		return Principal{}, apierr.New(http.StatusUnauthorized, "token_expired", "Your session has expired. Refresh or sign in again.")
	}
	return p, err
}

func (s *Service) SignOut(ctx context.Context, p Principal) error {
	_, err := s.store.revokeSession(ctx, p.UserID, p.SessionID, s.now())
	return err
}

func (s *Service) ChangePassword(ctx context.Context, p Principal, current, next string) error {
	u, err := s.store.userByID(ctx, p.UserID)
	if err != nil || u == nil {
		return err
	}
	if msg := checkPassword(next, u.Email); msg != "" {
		return apierr.Validation(map[string]string{"new_password": msg})
	}
	good, verr := s.hasher.Verify(current, u.PasswordHash)
	if verr != nil || !good {
		return apierr.Validation(map[string]string{"current_password": "Current password is not correct."})
	}
	hash, err := s.hasher.Hash(next)
	if err != nil {
		return err
	}
	return s.store.changePassword(ctx, u.ID, hash, p.SessionID, s.now())
}

// VerifyPassword checks a user's current password; used to confirm sensitive actions such as account deletion.
func (s *Service) VerifyPassword(ctx context.Context, userID, password string) error {
	u, err := s.store.userByID(ctx, userID)
	if err != nil || u == nil {
		return err
	}
	good, verr := s.hasher.Verify(password, u.PasswordHash)
	if verr != nil || !good {
		return apierr.Validation(map[string]string{"password": "Password is not correct."})
	}
	return nil
}

func (s *Service) ListSessions(ctx context.Context, p Principal) ([]SessionView, error) {
	rows, err := s.store.listSessions(ctx, p.UserID, s.now())
	if err != nil {
		return nil, err
	}
	out := make([]SessionView, 0, len(rows))
	for _, r := range rows {
		out = append(out, SessionView{ID: r.ID, DeviceName: r.DeviceName, Platform: r.Platform, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, Current: r.ID == p.SessionID})
	}
	return out, nil
}

func (s *Service) RevokeSession(ctx context.Context, p Principal, id string) error {
	ok, err := s.store.revokeSession(ctx, p.UserID, id, s.now())
	if err != nil {
		return err
	}
	if !ok {
		return apierr.ErrNotFound
	}
	return nil
}

func (s *Service) RevokeOthers(ctx context.Context, p Principal) error {
	n, err := s.store.revokeOthers(ctx, p.UserID, p.SessionID, s.now())
	if err == nil && n > 0 {
	}
	return err
}

func (s *Service) Purge(ctx context.Context) (int64, error) {
	return s.store.Purge(ctx, s.now())
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func validUUID(s string) bool { return uuidRe.MatchString(s) }

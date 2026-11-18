package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/app"
	"plinth/server/internal/platform/config"
	"plinth/server/internal/platform/security"
	"plinth/server/internal/platform/testdb"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	p, stop, err := testdb.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot start test database:", err)
		os.Exit(1)
	}
	pool = p
	code := m.Run()
	stop()
	os.Exit(code)
}

type env struct {
	srv *httptest.Server
}

func setup(t *testing.T) *env {
	t.Helper()
	testdb.Truncate(t, pool)
	cfg := config.Config{Env: "development", TokenPepper: []byte(strings.Repeat("p", 32)),
		AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour, RequestTimeout: 10 * time.Second}
	h, _ := app.New(app.Deps{Config: cfg, DB: pool, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hasher: security.Hasher{Time: 1, MemoryKiB: 64, Threads: 1}})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{srv: srv}
}

type resp struct {
	Status int
	Body   map[string]any
	Raw    string
}

func (e *env) do(t *testing.T, method, path, token string, body any) resp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{Status: res.StatusCode, Raw: string(raw)}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func (r resp) data() map[string]any { d, _ := r.Body["data"].(map[string]any); return d }
func (r resp) errCode() string {
	e, _ := r.Body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}
func str(m map[string]any, path ...string) string {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[p]
	}
	s, _ := cur.(string)
	return s
}

const pw = "correct horse battery"

func (e *env) signUp(t *testing.T, email string) (access, refresh, userID string) {
	t.Helper()
	r := e.do(t, "POST", "/v1/auth/sign-up", "", map[string]any{"email": email, "password": pw, "display_name": "Asha", "device_name": "Pixel 8", "platform": "android"})
	if r.Status != 201 {
		t.Fatalf("sign-up %d %s", r.Status, r.Raw)
	}
	d := r.data()
	return str(d, "session", "access_token"), str(d, "session", "refresh_token"), str(d, "user", "id")
}

func TestHealth(t *testing.T) {
	e := setup(t)
	if r := e.do(t, "GET", "/healthz", "", nil); r.Status != 200 {
		t.Fatalf("healthz %d", r.Status)
	}
	if r := e.do(t, "GET", "/readyz", "", nil); r.Status != 200 {
		t.Fatalf("readyz %d", r.Status)
	}
	if r := e.do(t, "GET", "/nope", "", nil); r.Status != 404 || r.errCode() != "not_found" {
		t.Fatalf("404 envelope: %d %s", r.Status, r.Raw)
	}
}

func TestSignUpValidationAndDuplicate(t *testing.T) {
	e := setup(t)
	r := e.do(t, "POST", "/v1/auth/sign-up", "", map[string]any{"email": "bad", "password": "short"})
	if r.Status != 422 || r.errCode() != "validation_failed" {
		t.Fatalf("want validation error, got %d %s", r.Status, r.Raw)
	}
	e.signUp(t, "asha@example.com")
	r = e.do(t, "POST", "/v1/auth/sign-up", "", map[string]any{"email": "ASHA@example.com", "password": pw})
	if r.Status != 409 || r.errCode() != "email_taken" {
		t.Fatalf("duplicate (case-insensitive) must be 409, got %d %s", r.Status, r.Raw)
	}
	if r := e.do(t, "POST", "/v1/auth/sign-up", "", map[string]any{"email": "x@example.com", "password": pw, "bogus": 1}); r.Status != 400 {
		t.Fatalf("unknown field must be rejected, got %d", r.Status)
	}
	var stored string
	_ = pool.QueryRow(context.Background(), `SELECT password_hash FROM users WHERE email='asha@example.com'`).Scan(&stored)
	if !strings.HasPrefix(stored, "$argon2id$") || strings.Contains(stored, pw) {
		t.Fatal("password not stored as argon2id hash")
	}
}

func TestSignInOutAndMe(t *testing.T) {
	e := setup(t)
	access, _, uid := e.signUp(t, "asha@example.com")

	if r := e.do(t, "GET", "/v1/users/me", "", nil); r.Status != 401 {
		t.Fatalf("no token must be 401, got %d", r.Status)
	}
	me := e.do(t, "GET", "/v1/users/me", access, nil)
	if me.Status != 200 || str(me.data(), "id") != uid {
		t.Fatalf("me: %d %s", me.Status, me.Raw)
	}
	if strings.Contains(me.Raw, "password") {
		t.Fatal("profile leaks credential fields")
	}

	bad := e.do(t, "POST", "/v1/auth/sign-in", "", map[string]any{"email": "asha@example.com", "password": "wrong password!!"})
	unknown := e.do(t, "POST", "/v1/auth/sign-in", "", map[string]any{"email": "ghost@example.com", "password": "wrong password!!"})
	if bad.Status != 401 || bad.Raw != unknown.Raw {
		t.Fatalf("wrong password and unknown email must be indistinguishable:\n%s\n%s", bad.Raw, unknown.Raw)
	}

	in := e.do(t, "POST", "/v1/auth/sign-in", "", map[string]any{"email": "asha@example.com", "password": pw, "device_name": "Tablet", "platform": "android"})
	if in.Status != 200 {
		t.Fatalf("sign-in %d %s", in.Status, in.Raw)
	}
	tok2 := str(in.data(), "session", "access_token")
	if r := e.do(t, "POST", "/v1/auth/sign-out", tok2, nil); r.Status != 204 {
		t.Fatalf("sign-out %d", r.Status)
	}
	if r := e.do(t, "GET", "/v1/users/me", tok2, nil); r.Status != 401 {
		t.Fatal("token must be dead right after sign-out")
	}
	if r := e.do(t, "GET", "/v1/users/me", access, nil); r.Status != 200 {
		t.Fatal("other session must survive")
	}
}

func TestLockoutAfterRepeatedFailures(t *testing.T) {
	e := setup(t)
	e.signUp(t, "asha@example.com")
	for i := 0; i < 5; i++ {
		e.do(t, "POST", "/v1/auth/sign-in", "", map[string]any{"email": "asha@example.com", "password": "nope nope nope"})
	}
	r := e.do(t, "POST", "/v1/auth/sign-in", "", map[string]any{"email": "asha@example.com", "password": pw})
	if r.Status != 401 {
		t.Fatalf("correct password must be refused while locked, got %d", r.Status)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE users SET locked_until = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if r := e.do(t, "POST", "/v1/auth/sign-in", "", map[string]any{"email": "asha@example.com", "password": pw}); r.Status != 200 {
		t.Fatalf("lock must expire, got %d", r.Status)
	}
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	e := setup(t)
	_, refresh1, _ := e.signUp(t, "asha@example.com")

	r := e.do(t, "POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh1})
	if r.Status != 200 {
		t.Fatalf("refresh %d %s", r.Status, r.Raw)
	}
	access2, refresh2 := str(r.data(), "access_token"), str(r.data(), "refresh_token")
	if refresh2 == refresh1 {
		t.Fatal("refresh token must rotate")
	}
	if e.do(t, "GET", "/v1/users/me", access2, nil).Status != 200 {
		t.Fatal("new access token must work")
	}
	// Replaying the old refresh token is theft evidence: the session must die.
	if r := e.do(t, "POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh1}); r.Status != 401 {
		t.Fatalf("reuse must be 401, got %d", r.Status)
	}
	if e.do(t, "GET", "/v1/users/me", access2, nil).Status != 401 {
		t.Fatal("session must be revoked after refresh reuse")
	}
	if e.do(t, "POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh2}).Status != 401 {
		t.Fatal("rotated-away session must stay revoked")
	}
}

func TestExpiredAccessTokenRejected(t *testing.T) {
	e := setup(t)
	access, _, _ := e.signUp(t, "asha@example.com")
	_, _ = pool.Exec(context.Background(), `UPDATE sessions SET access_expires_at = now() - interval '1 minute'`)
	r := e.do(t, "GET", "/v1/users/me", access, nil)
	if r.Status != 401 || r.errCode() != "token_expired" {
		t.Fatalf("got %d %s", r.Status, r.Raw)
	}
}

func TestChangePasswordKeepsCurrentSessionOnly(t *testing.T) {
	e := setup(t)
	access, _, _ := e.signUp(t, "asha@example.com")
	other := str(e.do(t, "POST", "/v1/auth/sign-in", "", map[string]any{"email": "asha@example.com", "password": pw}).data(), "session", "access_token")

	if r := e.do(t, "POST", "/v1/auth/change-password", access, map[string]any{"current_password": "wrong", "new_password": "another long passphrase"}); r.Status != 422 {
		t.Fatalf("wrong current password: %d", r.Status)
	}
	if r := e.do(t, "POST", "/v1/auth/change-password", access, map[string]any{"current_password": pw, "new_password": "another long passphrase"}); r.Status != 204 {
		t.Fatalf("change: %d %s", r.Status, r.Raw)
	}
	if e.do(t, "GET", "/v1/users/me", access, nil).Status != 200 {
		t.Fatal("current session must survive")
	}
	if e.do(t, "GET", "/v1/users/me", other, nil).Status != 401 {
		t.Fatal("other sessions must be revoked")
	}
}

func TestSessionsListAndRevoke_OwnershipEnforced(t *testing.T) {
	e := setup(t)
	a1, _, _ := e.signUp(t, "asha@example.com")
	b1, _, _ := e.signUp(t, "bala@example.com")
	a2 := str(e.do(t, "POST", "/v1/auth/sign-in", "", map[string]any{"email": "asha@example.com", "password": pw, "device_name": "Tablet"}).data(), "session", "access_token")

	list := e.do(t, "GET", "/v1/sessions", a1, nil)
	items, _ := list.Body["data"].([]any)
	if len(items) != 2 {
		t.Fatalf("want 2 sessions, got %d: %s", len(items), list.Raw)
	}
	var otherID string
	for _, it := range items {
		m := it.(map[string]any)
		if m["current"] == false {
			otherID = m["id"].(string)
		}
	}
	// Another user cannot revoke Asha's session (IDOR check).
	if r := e.do(t, "DELETE", "/v1/sessions/"+otherID, b1, nil); r.Status != 404 {
		t.Fatalf("cross-user revoke must be 404, got %d", r.Status)
	}
	if e.do(t, "GET", "/v1/users/me", a2, nil).Status != 200 {
		t.Fatal("session was revoked by another user")
	}
	if r := e.do(t, "DELETE", "/v1/sessions/not-a-uuid", a1, nil); r.Status != 404 {
		t.Fatalf("bad id: %d", r.Status)
	}
	if r := e.do(t, "DELETE", "/v1/sessions/"+otherID, a1, nil); r.Status != 204 {
		t.Fatalf("own revoke: %d", r.Status)
	}
	if e.do(t, "GET", "/v1/users/me", a2, nil).Status != 401 {
		t.Fatal("revoked session must stop working")
	}
}

func TestProfileUpdateAndDeletionRequest(t *testing.T) {
	e := setup(t)
	access, _, _ := e.signUp(t, "asha@example.com")

	r := e.do(t, "PUT", "/v1/users/me", access, map[string]any{"display_name": "Asha K"})
	if r.Status != 200 || str(r.data(), "display_name") != "Asha K" {
		t.Fatalf("update: %d %s", r.Status, r.Raw)
	}
	if r := e.do(t, "PUT", "/v1/users/me", access, map[string]any{"display_name": strings.Repeat("x", 101)}); r.Status != 422 {
		t.Fatalf("long name: %d", r.Status)
	}
	if r := e.do(t, "POST", "/v1/users/me/deletion-request", access, map[string]any{"password": "wrong"}); r.Status != 422 {
		t.Fatalf("deletion needs password: %d", r.Status)
	}
	r = e.do(t, "POST", "/v1/users/me/deletion-request", access, map[string]any{"password": pw})
	if r.Status != 202 || r.data()["deletion_requested_at"] == nil {
		t.Fatalf("deletion request: %d %s", r.Status, r.Raw)
	}
	r = e.do(t, "DELETE", "/v1/users/me/deletion-request", access, nil)
	if r.Status != 200 || r.data()["deletion_requested_at"] != nil {
		t.Fatalf("cancel: %d %s", r.Status, r.Raw)
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE event LIKE 'user.deletion%'`).Scan(&n)
	if n != 2 {
		t.Fatalf("audit events: %d", n)
	}
}

func TestSettings(t *testing.T) {
	e := setup(t)
	access, _, _ := e.signUp(t, "asha@example.com")

	g := e.do(t, "GET", "/v1/settings", access, nil)
	if g.Status != 200 || str(g.data(), "currency") != "INR" || str(g.data(), "theme") != "system" || str(g.data(), "number_format") != "indian" {
		t.Fatalf("defaults: %d %s", g.Status, g.Raw)
	}
	p := e.do(t, "PATCH", "/v1/settings", access, map[string]any{"currency": "USD", "theme": "dark", "timezone": "America/New_York", "number_format": "international", "hide_amounts": true})
	if p.Status != 200 || str(p.data(), "currency") != "USD" || p.data()["hide_amounts"] != true || p.data()["version"].(float64) != 2 {
		t.Fatalf("patch: %d %s", p.Status, p.Raw)
	}
	if str(p.data(), "date_format") != "dmy" {
		t.Fatal("untouched fields must keep their value")
	}
	for name, body := range map[string]map[string]any{
		"currency": {"currency": "XXX"}, "theme": {"theme": "neon"}, "timezone": {"timezone": "Mars/Base"}, "date_format": {"date_format": "x"},
	} {
		if r := e.do(t, "PATCH", "/v1/settings", access, body); r.Status != 422 {
			t.Errorf("%s: want 422 got %d", name, r.Status)
		}
	}
	// Settings are per user.
	b, _, _ := e.signUp(t, "bala@example.com")
	if str(e.do(t, "GET", "/v1/settings", b, nil).data(), "currency") != "INR" {
		t.Fatal("settings leaked across users")
	}
	if e.do(t, "GET", "/v1/settings", "", nil).Status != 401 {
		t.Fatal("settings need auth")
	}
}

func TestAuthRateLimit(t *testing.T) {
	e := setup(t)
	limited := false
	for i := 0; i < 40; i++ {
		if e.do(t, "POST", "/v1/auth/sign-in", "", map[string]any{"email": "x@example.com", "password": "whatever long"}).Status == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("sign-in is not rate limited")
	}
}

func TestSecurityHeadersAndNoStore(t *testing.T) {
	e := setup(t)
	res, err := http.Get(e.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("X-Request-ID") == "" {
		t.Fatalf("headers: %v", res.Header)
	}
}

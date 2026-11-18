package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:5555"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7")
	if got := ClientIP(r, false); got != "203.0.113.9" {
		t.Errorf("untrusted proxy: %s", got)
	}
	if got := ClientIP(r, true); got != "198.51.100.7" {
		t.Errorf("trusted proxy must use rightmost entry: %s", got)
	}
}

func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter(60, 2)
	if !rl.Allow("a") || !rl.Allow("a") || rl.Allow("a") {
		t.Fatal("burst of 2 not enforced")
	}
	if !rl.Allow("b") {
		t.Fatal("keys must be independent")
	}
}

func TestDecodeStrict(t *testing.T) {
	var v struct {
		A string `json:"a"`
	}
	mk := func(body string) *http.Request { return httptest.NewRequest("POST", "/", strings.NewReader(body)) }
	if err := Decode(mk(`{"a":"x"}`), &v); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"a":"x","b":1}`, `{"a":"x"}{"a":"y"}`, `nope`} {
		if err := Decode(mk(bad), &v); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestRecoverAndRequestID(t *testing.T) {
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
		RequestIDMiddleware, Recover, Limit(1024, time.Second))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 500 || w.Header().Get("X-Request-ID") == "" || strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

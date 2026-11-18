package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"plinth/server/internal/platform/apierr"
)

type ctxKey int

const requestIDKey ctxKey = 1

// IdempotencyHeader is the request header for safe retries of financial writes (enforced from Phase 2).
const IdempotencyHeader = "Idempotency-Key"

var idPattern = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !idPattern.MatchString(id) {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.ErrorContext(r.Context(), "panic", "panic", v, "stack", string(debug.Stack()), "request_id", RequestID(r.Context()))
				Error(w, r, apierr.ErrInternal)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusWriter) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }
func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// AccessLog logs method, route pattern, status and duration. It never logs query strings, headers or bodies.
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			log.Info("request", "method", r.Method, "route", route, "status", sw.status,
				"ms", time.Since(start).Milliseconds(), "bytes", sw.bytes, "request_id", RequestID(r.Context()))
		})
	}
}

func SecurityHeaders(production bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Cache-Control", "no-store")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Referrer-Policy", "no-referrer")
			if production {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Limit caps the body size and applies a deadline to the request context.
func Limit(maxBody int64, timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ClientIP returns the caller address. Forwarded headers are honoured only when trustProxy is set, and
// then the right-most X-Forwarded-For entry (appended by our own proxy) is used.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimiter is a per-key token bucket held in memory. It protects a single instance; put a shared limiter
// at the edge if the server is scaled out.
type RateLimiter struct {
	mu      sync.Mutex
	entries map[string]*entry
	limit   rate.Limit
	burst   int
}
type entry struct {
	l    *rate.Limiter
	seen time.Time
}

func NewRateLimiter(perMinute float64, burst int) *RateLimiter {
	return &RateLimiter{entries: map[string]*entry{}, limit: rate.Limit(perMinute / 60), burst: burst}
}

func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	if len(rl.entries) > 10000 { // bound memory
		for k, e := range rl.entries {
			if now.Sub(e.seen) > 10*time.Minute {
				delete(rl.entries, k)
			}
		}
	}
	e, ok := rl.entries[key]
	if !ok {
		e = &entry{l: rate.NewLimiter(rl.limit, rl.burst)}
		rl.entries[key] = e
	}
	e.seen = now
	return e.l.Allow()
}

func (rl *RateLimiter) Middleware(trustProxy bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !rl.Allow(ClientIP(r, trustProxy)) {
				Error(w, r, apierr.ErrRateLimited)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Chain applies middleware so the first listed is outermost.
func Chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

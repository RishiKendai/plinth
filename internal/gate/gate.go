// Package gate answers two questions before any API work happens: is the service in maintenance, and is the app too old
// to talk to this server. Both are decided by configuration, so an operator can flip them without a deploy of code.
package gate

import (
	"net/http"
	"strconv"
	"strings"

	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/httpx"
)

type Config struct {
	Maintenance      bool
	MinClientVersion string // dotted numbers, e.g. "0.2.0"; empty means every version is accepted
	ServerVersion    string
}

// Compare returns -1, 0 or 1 comparing dotted numeric versions. Non-numeric suffixes ("-debug") are ignored; a malformed
// version compares as 0.0.0 so it can only ever be refused, never crash.
func Compare(a, b string) int {
	pa, pb := parts(a), parts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parts(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	for i, p := range strings.SplitN(v, ".", 3) {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}
		}
		out[i] = n
	}
	return out
}

var (
	errMaintenance = apierr.New(http.StatusServiceUnavailable, "maintenance", "Plinth is being updated for a short while. Nothing is lost. Try again in a few minutes.")
	errUpdate      = apierr.New(http.StatusUpgradeRequired, "update_required", "This version of Plinth is too old to keep working. Update the app to continue. Your records are safe.")
)

// Middleware guards /v1 routes. /healthz, /readyz and /v1/meta always answer, so the app can learn why it is blocked.
// A request with no X-Client-Version header is allowed (curl, tests, very old builds are refused by the maintenance flag only).
func Middleware(c Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/v1/meta" {
				next.ServeHTTP(w, r)
				return
			}
			if c.Maintenance {
				w.Header().Set("Retry-After", "300")
				httpx.Error(w, r, errMaintenance)
				return
			}
			if v := r.Header.Get("X-Client-Version"); v != "" && c.MinClientVersion != "" && Compare(v, c.MinClientVersion) < 0 {
				httpx.Error(w, r, errUpdate)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Meta is the public status document.
func Meta(c Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]any{
			"server_version": c.ServerVersion, "min_client_version": c.MinClientVersion, "maintenance": c.Maintenance,
		})
	}
}

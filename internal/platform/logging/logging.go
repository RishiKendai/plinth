// Package logging builds the structured logger. It redacts sensitive attribute keys (CLAUDE.md section 8).
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

var sensitive = []string{"password", "token", "authorization", "secret", "cookie", "pepper", "code", "otp", "pin", "dsn", "database_url", "url", "presign", "sms", "content", "fingerprint"}

const Redacted = "[REDACTED]"

func isSensitive(key string) bool {
	k := strings.ToLower(key)
	if strings.HasSuffix(k, "_id") || k == "id" {
		return false
	}
	for _, s := range sensitive {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

func New(w io.Writer, level string, production bool) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if isSensitive(a.Key) {
				return slog.String(a.Key, Redacted)
			}
			return a
		},
	}
	if w == nil {
		w = os.Stdout
	}
	if production {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedaction(t *testing.T) {
	var b bytes.Buffer
	l := New(&b, "debug", true)
	l.Info("x", "password", "hunter2", "refresh_token", "abc", "Authorization", "Bearer z", "user_id", "u-1", "request_id", "r-1")
	out := b.String()
	for _, leaked := range []string{"hunter2", "abc", "Bearer z"} {
		if strings.Contains(out, leaked) {
			t.Errorf("leaked %q in %s", leaked, out)
		}
	}
	for _, kept := range []string{"u-1", "r-1"} {
		if !strings.Contains(out, kept) {
			t.Errorf("over-redacted %q in %s", kept, out)
		}
	}
}

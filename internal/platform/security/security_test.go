package security

import (
	"strings"
	"testing"
)

func fast() Hasher { return Hasher{Time: 1, MemoryKiB: 64, Threads: 1} }

func TestPasswordRoundTrip(t *testing.T) {
	h := fast()
	enc, err := h.Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "correct") {
		t.Fatal("hash contains plaintext")
	}
	if ok, err := h.Verify("correct horse battery", enc); !ok || err != nil {
		t.Fatalf("verify good: %v %v", ok, err)
	}
	if ok, _ := h.Verify("wrong", enc); ok {
		t.Fatal("verified wrong password")
	}
	enc2, _ := h.Hash("correct horse battery")
	if enc == enc2 {
		t.Fatal("salt not random")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "plain", "$argon2id$v=19$m=999999999,t=1,p=1$AAAA$AAAA", "$bcrypt$a$b$c$d"} {
		if _, err := fast().Verify("x", bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestTokens(t *testing.T) {
	a, _ := NewToken("pat_")
	b, _ := NewToken("pat_")
	if a == b || !strings.HasPrefix(a, "pat_") || len(a) < 40 {
		t.Fatalf("bad tokens %q %q", a, b)
	}
	p := []byte(strings.Repeat("k", 32))
	if string(HashToken(p, a)) == string(HashToken(p, b)) {
		t.Fatal("collision")
	}
	if string(HashToken(p, "ab", "c")) == string(HashToken(p, "a", "bc")) {
		t.Fatal("parts not delimited")
	}
	for i := 0; i < 200; i++ {
		c, _ := NewCode()
		if len(c) != 6 {
			t.Fatalf("code %q", c)
		}
	}
}

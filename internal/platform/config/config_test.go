package config

import (
	"strings"
	"testing"
	"time"
)

func valid() Config {
	return Config{
		Env: "development", DatabaseURL: "postgresql://u:p@host/db", DBMaxConns: 5,
		TokenPepper: []byte(strings.Repeat("a", 32)), AccessTokenTTL: time.Minute,
	}
}

func TestValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	c := valid()
	c.DatabaseURL = "postgresql://CHANGE_ME:CHANGE_ME@ep-CHANGE_ME/neondb"
	if err := c.Validate(); err == nil || strings.Contains(err.Error(), "CHANGE_ME@") {
		t.Fatalf("placeholder DB url must fail without echoing it, got %v", err)
	}
	c = valid()
	c.TokenPepper = []byte("short")
	if c.Validate() == nil {
		t.Fatal("short pepper accepted")
	}
}

func TestParseLine(t *testing.T) {
	cases := []struct {
		in, k, v string
		ok       bool
	}{
		{`A=1`, "A", "1", true},
		{`  export B = "x y" `, "B", "x y", true},
		{`# c`, "", "", false},
		{``, "", "", false},
		{`C='q'`, "C", "q", true},
		{`D=a=b`, "D", "a=b", true},
	}
	for _, c := range cases {
		k, v, ok := parseLine(c.in)
		if k != c.k || v != c.v || ok != c.ok {
			t.Errorf("parseLine(%q) = %q,%q,%v", c.in, k, v, ok)
		}
	}
}

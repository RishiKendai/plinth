package money

import (
	"encoding/json"
	"os"
	"testing"
)

type vectors struct {
	Format []struct {
		Minor        int64  `json:"minor"`
		Currency     string `json:"currency"`
		Grouping     string `json:"grouping"`
		HideDecimals bool   `json:"hideDecimals"`
		Sign         string `json:"sign"`
		Expected     string `json:"expected"`
	} `json:"format"`
	Parse []struct {
		Input    string `json:"input"`
		Currency string `json:"currency"`
		Minor    int64  `json:"minor"`
		Error    string `json:"error"`
	} `json:"parse"`
}

func load(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("../../../api/testdata/money.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFormatVectors(t *testing.T) {
	for _, c := range load(t).Format {
		cur, ok := Lookup(c.Currency)
		if !ok {
			t.Fatalf("unknown currency %s", c.Currency)
		}
		o := Options{HideDecimals: c.HideDecimals}
		if c.Grouping == "indian" {
			o.Grouping = Indian
		}
		switch c.Sign {
		case "always":
			o.Sign = SignAlways
		case "never":
			o.Sign = SignNever
		}
		if got := Format(c.Minor, cur, o); got != c.Expected {
			t.Errorf("Format(%d,%s) = %q, want %q", c.Minor, c.Currency, got, c.Expected)
		}
	}
}

func TestParseVectors(t *testing.T) {
	for _, c := range load(t).Parse {
		cur, _ := Lookup(c.Currency)
		got, err := Parse(c.Input, cur)
		if c.Error != "" {
			if err == nil || err.Error() != c.Error {
				t.Errorf("Parse(%q) err = %v, want %s", c.Input, err, c.Error)
			}
			continue
		}
		if err != nil || got != c.Minor {
			t.Errorf("Parse(%q) = %d, %v; want %d", c.Input, got, err, c.Minor)
		}
	}
}

func TestMinInt64(t *testing.T) {
	cur, _ := Lookup("INR")
	if s := Format(-1<<63, cur, Options{Grouping: Indian}); s == "" {
		t.Fatal("empty")
	}
}

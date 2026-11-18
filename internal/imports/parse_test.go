package imports

import "testing"

func TestParseAmount(t *testing.T) {
	cases := []struct {
		in   string
		exp  int
		want int64
		neg  bool
		dir  string
		bad  bool
	}{
		{"1,234.50", 2, 123450, false, "", false},
		{"12,34,567.89", 2, 123456789, false, "", false},
		{"-482.00", 2, 48200, true, "", false},
		{"(1,000.00)", 2, 100000, true, "", false},
		{"₹ 1250.5", 2, 125050, false, "", false},
		{"Rs. 99", 2, 9900, false, "", false},
		{"INR 10.25", 2, 1025, false, "", false},
		{"2,500.00 Dr", 2, 250000, false, "debit", false},
		{"2,500.00 Cr.", 2, 250000, false, "credit", false},
		{"0.05", 2, 5, false, "", false},
		{"0", 2, 0, false, "", false},
		{"500", 0, 500, false, "", false},
		{"1.234", 3, 1234, false, "", false},
		{"10.500", 2, 1050, false, "", false}, // a trailing zero beyond the exponent is fine
		{"10.505", 2, 0, false, "", true},     // would lose a fraction of a paisa
		{"abc", 2, 0, false, "", true},
		{"", 2, 0, false, "", true},
		{"1.2.3", 2, 0, false, "", true},
		{"99999999999999999999", 2, 0, false, "", true},
		{".5", 2, 50, false, "", false},
	}
	for _, c := range cases {
		got, neg, dir, err := ParseAmount(c.in, c.exp)
		if (err != nil) != c.bad {
			t.Errorf("%q: err=%v bad=%v", c.in, err, c.bad)
			continue
		}
		if !c.bad && (got != c.want || neg != c.neg || dir != c.dir) {
			t.Errorf("%q: got %d neg=%v dir=%q want %d neg=%v dir=%q", c.in, got, neg, dir, c.want, c.neg, c.dir)
		}
	}
}

func TestParseDate(t *testing.T) {
	cases := []struct{ in, format, want string }{
		{"2026-11-18", "dmy", "2026-11-18"},
		{"18/11/2026", "dmy", "2026-11-18"},
		{"18-11-2026", "", "2026-11-18"},
		{"11/18/2026", "mdy", "2026-11-18"},
		{"03/04/2026", "dmy", "2026-04-03"}, // ambiguous: the chosen order decides
		{"03/04/2026", "mdy", "2026-03-04"},
		{"18 Nov 2026", "dmy", "2026-11-18"},
		{"18-Nov-26", "dmy", "2026-11-18"},
		{"2026-11-18T10:30:00", "dmy", "2026-11-18"},
		{"2026-11-18 10:30", "dmy", "2026-11-18"},
		{"29/02/2028", "dmy", "2028-02-29"},
	}
	for _, c := range cases {
		d, err := ParseDate(c.in, c.format)
		if err != nil || d.Format("2006-01-02") != c.want {
			t.Errorf("%q (%s): got %v %v want %s", c.in, c.format, d, err, c.want)
		}
	}
	for _, bad := range []string{"31/02/2026", "29/02/2027", "13/13/2026", "", "yesterday", "2026-13-01"} {
		if _, err := ParseDate(bad, "dmy"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestNormalizeAndSafeCell(t *testing.T) {
	if Normalize("SWIGGY *Order #1!") != Normalize("swiggy order 1") {
		t.Fatal("normalize")
	}
	for in, want := range map[string]string{"=SUM(A1)": "'=SUM(A1)", "+91 98": "'+91 98", "-5": "'-5", "@x": "'@x", "Swiggy": "Swiggy", "": ""} {
		if got := SafeCell(in); got != want {
			t.Errorf("SafeCell(%q)=%q want %q", in, got, want)
		}
	}
}

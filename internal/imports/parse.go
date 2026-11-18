package imports

import (
	"errors"
	"strings"
	"time"
	"unicode"
)

// ParseAmount reads text such as "1,234.50", "(1,234.50)", "-₹ 1234.5", "1234.50 Dr" into minor units without any
// floating point. neg is true for a minus sign or parentheses; dir is "debit" or "credit" when the text carries a
// Dr/Cr marker, otherwise "".
func ParseAmount(s string, exponent int) (minor int64, neg bool, dir string, err error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false, "", errors.New("empty amount")
	}
	low := strings.ToLower(t)
	for _, suf := range []struct{ s, d string }{{"dr.", "debit"}, {"cr.", "credit"}, {"dr", "debit"}, {"cr", "credit"}} {
		if strings.HasSuffix(low, suf.s) {
			dir = suf.d
			t = strings.TrimSpace(t[:len(t)-len(suf.s)])
			break
		}
	}
	if strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
		neg = true
		t = t[1 : len(t)-1]
	}
	var b strings.Builder
	runes := []rune(t)
	var prev rune
	for i, r := range runes {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.':
			// A dot after a currency word ("Rs. 99") is punctuation, not a decimal point.
			if unicode.IsLetter(prev) && b.Len() == 0 {
				break
			}
			b.WriteRune(r)
		case r == '-' || r == '−':
			neg = true
		case r == ',' || r == '₹' || r == '$' || r == '€' || r == '£' || unicode.IsSpace(r):
		case unicode.IsLetter(r):
			// letters such as "Rs" or "INR" in front of the number
		default:
			return 0, false, "", errors.New("not a number")
		}
		_ = i
		prev = r
	}
	num := b.String()
	whole, frac, hasDot := strings.Cut(num, ".")
	if num == "" || strings.Contains(frac, ".") || (whole == "" && frac == "") || (hasDot && whole == "" && frac == "") {
		return 0, false, "", errors.New("not a number")
	}
	if len(frac) > exponent {
		extra := frac[exponent:]
		if strings.Trim(extra, "0") != "" {
			return 0, false, "", errors.New("too many decimal places")
		}
		frac = frac[:exponent]
	}
	frac += strings.Repeat("0", exponent-len(frac))
	digits := strings.TrimLeft(whole+frac, "0")
	if digits == "" {
		return 0, neg, dir, nil
	}
	if len(digits) > 18 {
		return 0, false, "", errors.New("amount too large")
	}
	var v int64
	for _, c := range digits {
		v = v*10 + int64(c-'0')
	}
	return v, neg, dir, nil
}

var layouts = map[string][]string{
	"ymd": {"2006-01-02", "2006/01/02", "2006.01.02"},
	"dmy": {"02/01/2006", "2/1/2006", "02-01-2006", "2-1-2006", "02.01.2006", "02/01/06", "2/1/06", "02-01-06"},
	"mdy": {"01/02/2006", "1/2/2006", "01-02-2006", "1-2-2006", "01/02/06", "1/2/06"},
	"txt": {"02 Jan 2006", "2 Jan 2006", "02-Jan-2006", "2-Jan-2006", "02 January 2006", "2 January 2006", "Jan 2, 2006", "January 2, 2006", "02-Jan-06", "2-Jan-06", "02 Jan 06"},
}

// ParseDate reads a date in the given format ("dmy" is the default, since the product is Indian-first; "mdy" and
// "ymd" are options). Dates written with a month name or a leading four-digit year are unambiguous and accepted in
// any mode. A date that does not exist (31/02) is an error. A timestamp suffix is ignored.
func ParseDate(s, format string) (time.Time, error) {
	t := strings.TrimSpace(s)
	if i := strings.IndexAny(t, "T "); i > 0 && strings.Count(t[:i], "-") == 2 && strings.Contains(t[i:], ":") {
		t = t[:i]
	}
	order := []string{"ymd", "txt"}
	switch format {
	case "mdy":
		order = append(order, "mdy", "dmy")
	default:
		order = append(order, "dmy", "mdy")
	}
	if format == "dmy" || format == "" || format == "auto" {
		order = []string{"ymd", "txt", "dmy"}
	}
	if format == "mdy" {
		order = []string{"ymd", "txt", "mdy"}
	}
	for _, key := range order {
		for _, l := range layouts[key] {
			if d, err := time.Parse(l, t); err == nil {
				return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC), nil
			}
		}
	}
	return time.Time{}, errors.New("not a valid date")
}

// Normalize lower-cases, drops punctuation and collapses spaces so "SWIGGY *Order #1" and "swiggy order 1" compare
// equal when looking for duplicates.
func Normalize(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// SafeCell neutralises spreadsheet formula injection in exported files: a cell that starts with = + - @ tab or CR is
// prefixed with an apostrophe so Excel and Sheets show it as text.
func SafeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

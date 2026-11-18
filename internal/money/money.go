// Package money holds currency-aware integer arithmetic helpers (CLAUDE.md sections 3.5 and 5.3).
// Amounts are int64 minor units. Floats are never used. The same behaviour is implemented in
// Kotlin (client/shared) and both are checked against api/testdata/money.json.
package money

import (
	"errors"
	"math"
	"strings"
)

type Currency struct {
	Code     string
	Exponent int // digits after the decimal point
	Symbol   string
}

var currencies = map[string]Currency{}

func init() {
	for _, c := range []Currency{
		{"INR", 2, "₹"}, {"USD", 2, "$"}, {"EUR", 2, "€"}, {"GBP", 2, "£"}, {"JPY", 0, "¥"},
		{"AED", 2, "AED"}, {"AUD", 2, "A$"}, {"CAD", 2, "C$"}, {"CHF", 2, "CHF"}, {"CNY", 2, "CN¥"},
		{"SGD", 2, "S$"}, {"HKD", 2, "HK$"}, {"NZD", 2, "NZ$"}, {"SEK", 2, "SEK"}, {"NOK", 2, "NOK"},
		{"DKK", 2, "DKK"}, {"ZAR", 2, "R"}, {"BRL", 2, "R$"}, {"MXN", 2, "MX$"}, {"THB", 2, "฿"},
		{"MYR", 2, "RM"}, {"IDR", 2, "Rp"}, {"PHP", 2, "₱"}, {"KRW", 0, "₩"}, {"VND", 0, "₫"},
		{"LKR", 2, "Rs"}, {"NPR", 2, "Rs"}, {"BDT", 2, "৳"}, {"PKR", 2, "Rs"}, {"SAR", 2, "SAR"},
		{"QAR", 2, "QAR"}, {"KWD", 3, "KWD"}, {"BHD", 3, "BHD"}, {"OMR", 3, "OMR"}, {"JOD", 3, "JOD"},
		{"TRY", 2, "₺"}, {"RUB", 2, "₽"}, {"PLN", 2, "zł"}, {"CZK", 2, "Kč"}, {"ILS", 2, "₪"},
		{"EGP", 2, "E£"}, {"NGN", 2, "₦"}, {"KES", 2, "KSh"},
	} {
		currencies[c.Code] = c
	}
}

func Lookup(code string) (Currency, bool) {
	c, ok := currencies[code]
	return c, ok
}

// Codes returns every supported ISO 4217 code (unordered).
func Codes() []string {
	out := make([]string, 0, len(currencies))
	for k := range currencies {
		out = append(out, k)
	}
	return out
}

type Grouping int

const (
	International Grouping = iota // 1,234,567
	Indian                        // 12,34,567
)

type SignMode int

const (
	SignNegativeOnly SignMode = iota // "−" for negatives only
	SignAlways                       // "+" or "−"
	SignNever                        // magnitude only
)

type Options struct {
	Grouping     Grouping
	HideDecimals bool // drop the fractional part (display only; truncates toward zero)
	Sign         SignMode
}

const minus = "−" // true minus sign, DESIGN.md section 10

// Format renders "₹ 1,284.00" style text. Sign precedes the symbol: "− ₹ 1,284.00".
func Format(minor int64, c Currency, o Options) string {
	neg := minor < 0
	mag := uint64(minor)
	if neg {
		mag = uint64(-(minor + 1)) + 1 // safe for MinInt64
	}
	div := uint64(1)
	for i := 0; i < c.Exponent; i++ {
		div *= 10
	}
	whole, frac := mag/div, mag%div

	var b strings.Builder
	switch {
	case o.Sign == SignAlways && neg, o.Sign == SignNegativeOnly && neg:
		b.WriteString(minus + " ")
	case o.Sign == SignAlways:
		b.WriteString("+ ")
	}
	b.WriteString(c.Symbol)
	b.WriteString(" ")
	b.WriteString(group(utoa(whole), o.Grouping))
	if c.Exponent > 0 && !o.HideDecimals {
		fs := utoa(frac)
		b.WriteString("." + strings.Repeat("0", c.Exponent-len(fs)) + fs)
	}
	return b.String()
}

func utoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func group(digits string, g Grouping) string {
	if len(digits) <= 3 {
		return digits
	}
	head, tail := digits[:len(digits)-3], digits[len(digits)-3:]
	size := 3
	if g == Indian {
		size = 2
	}
	var parts []string
	for len(head) > size {
		parts = append([]string{head[len(head)-size:]}, parts...)
		head = head[:len(head)-size]
	}
	parts = append([]string{head}, parts...)
	return strings.Join(parts, ",") + "," + tail
}

var (
	ErrEmpty        = errors.New("empty")
	ErrInvalid      = errors.New("invalid")
	ErrTooManyDecs  = errors.New("too_many_decimals")
	ErrOutOfRange   = errors.New("out_of_range")
	ErrUnknownCurry = errors.New("unknown_currency")
)

// Parse converts user text such as "1,284.50" into minor units without floating point.
// Commas are ignored. A leading "-" or "−" is allowed. More decimals than the currency allows is an error.
func Parse(s string, c Currency) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrEmpty
	}
	neg := false
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, minus) {
		neg = true
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "-"), minus))
	}
	s = strings.ReplaceAll(s, ",", "")
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" && (!hasDot || frac == "") {
		return 0, ErrInvalid
	}
	if !digitsOnly(whole) || !digitsOnly(frac) {
		return 0, ErrInvalid
	}
	if len(frac) > c.Exponent {
		return 0, ErrTooManyDecs
	}
	frac += strings.Repeat("0", c.Exponent-len(frac))
	var v uint64
	for _, ch := range whole + frac {
		d := uint64(ch - '0')
		if v > (math.MaxInt64-d)/10 {
			return 0, ErrOutOfRange
		}
		v = v*10 + d
	}
	if neg {
		return -int64(v), nil
	}
	return int64(v), nil
}

func digitsOnly(s string) bool {
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

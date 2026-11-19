package imports

import (
	"strings"
	"testing"
)

func BenchmarkParseAmount(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _, _ = ParseAmount("1,12,400.50 Dr", 2)
	}
}

func BenchmarkParseDate(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = ParseDate("18/11/2026", "dmy")
	}
}

func BenchmarkNormalizeTitle(b *testing.B) {
	s := "UPI-SWIGGY-swiggy@icici-432198765432 PAYMENT FROM PHONE"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Normalize(s)
	}
}

func BenchmarkSafeCell(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = SafeCell(strings.Repeat("x", 40))
	}
}

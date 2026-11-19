package groups

import (
	"fmt"
	"testing"
)

func parts(n int) []Part {
	p := make([]Part, n)
	for i := range p {
		p[i] = Part{Member: fmt.Sprintf("00000000-0000-4000-8000-%012d", i), Value: int64(i%7 + 1)}
	}
	return p
}

func BenchmarkComputeEqual10(b *testing.B) {
	p := parts(10)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = Compute("equal", 1_000_001, p)
	}
}

func BenchmarkComputeShares50(b *testing.B) {
	p := parts(50)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = Compute("shares", 12_345_678, p)
	}
}

func net(n int) map[string]int64 {
	m := map[string]int64{}
	var sum int64
	for i := 0; i < n-1; i++ {
		v := int64((i*7919)%20000 - 10000)
		m[fmt.Sprintf("m%03d", i)] = v
		sum += v
	}
	m[fmt.Sprintf("m%03d", n-1)] = -sum
	return m
}

func BenchmarkSimplify8(b *testing.B) {
	m := net(8)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = Simplify(m)
	}
}

// 14 is the largest group the exact subset search handles; above it the greedy fallback runs.
func BenchmarkSimplify14Exact(b *testing.B) {
	m := net(14)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = Simplify(m)
	}
}

func BenchmarkSimplify200Greedy(b *testing.B) {
	m := net(200)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = Simplify(m)
	}
}

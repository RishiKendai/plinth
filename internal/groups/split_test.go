package groups

import (
	"math/rand"
	"reflect"
	"testing"
)

func sum(s []Share) (t int64) {
	for _, x := range s {
		t += x.Amount
	}
	return
}

func amounts(s []Share) []int64 {
	var o []int64
	for _, x := range s {
		o = append(o, x.Amount)
	}
	return o
}

func TestEqualSplitRoundsDeterministically(t *testing.T) {
	// 100.00 / 3: 33.34, 33.33, 33.33. The extra paisa goes to the first member by identifier, always.
	s, err := Compute("equal", 10000, []Part{{"c", 0}, {"a", 0}, {"b", 0}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(amounts(s), []int64{3334, 3333, 3333}) || s[0].Member != "a" {
		t.Fatalf("got %+v", s)
	}
	// Input order must not matter.
	s2, _ := Compute("equal", 10000, []Part{{"a", 0}, {"b", 0}, {"c", 0}})
	if !reflect.DeepEqual(s, s2) {
		t.Fatal("order changed the result")
	}
	one, _ := Compute("equal", 1, []Part{{"a", 0}, {"b", 0}, {"c", 0}})
	if !reflect.DeepEqual(amounts(one), []int64{1, 0, 0}) {
		t.Fatalf("1 paisa among 3: %v", amounts(one))
	}
}

func TestPercentAndSharesLargestRemainder(t *testing.T) {
	// 33.33% x3 = 99.99%: invalid. 3333+3333+3334 = 10000 works.
	if _, err := Compute("percent", 10000, []Part{{"a", 3333}, {"b", 3333}, {"c", 3333}}); err == nil {
		t.Fatal("99.99% accepted")
	}
	s, err := Compute("percent", 10000, []Part{{"a", 3333}, {"b", 3333}, {"c", 3334}})
	if err != nil || sum(s) != 10000 || !reflect.DeepEqual(amounts(s), []int64{3333, 3333, 3334}) {
		t.Fatalf("%v %v", s, err)
	}
	// 100 paise by shares 1:1:1 -> 34,33,33
	s, _ = Compute("shares", 100, []Part{{"a", 1}, {"b", 1}, {"c", 1}})
	if !reflect.DeepEqual(amounts(s), []int64{34, 33, 33}) {
		t.Fatalf("%v", amounts(s))
	}
	// 2:1 on 100 -> 66.67 / 33.33 (remainder .67 vs .33 -> the first gets the extra)
	s, _ = Compute("shares", 100, []Part{{"a", 2}, {"b", 1}})
	if !reflect.DeepEqual(amounts(s), []int64{67, 33}) {
		t.Fatalf("%v", amounts(s))
	}
}

func TestExactMustReconcile(t *testing.T) {
	if _, err := Compute("exact", 10000, []Part{{"a", 6000}, {"b", 3999}}); err == nil {
		t.Fatal("short by one paisa accepted")
	}
	if _, err := Compute("exact", 10000, []Part{{"a", 6000}, {"b", 4001}}); err == nil {
		t.Fatal("over by one paisa accepted")
	}
	s, err := Compute("exact", 10000, []Part{{"a", 6000}, {"b", 4000}})
	if err != nil || sum(s) != 10000 {
		t.Fatalf("%v %v", s, err)
	}
	if _, err := Compute("exact", 10000, []Part{{"a", -5}, {"b", 10005}}); err == nil {
		t.Fatal("negative accepted")
	}
}

func TestInvalidInputs(t *testing.T) {
	for name, f := range map[string]func() error{
		"zero total": func() error { _, e := Compute("equal", 0, []Part{{"a", 0}}); return e },
		"no people":  func() error { _, e := Compute("equal", 10, nil); return e },
		"duplicate":  func() error { _, e := Compute("equal", 10, []Part{{"a", 0}, {"a", 0}}); return e },
		"bad method": func() error { _, e := Compute("random", 10, []Part{{"a", 0}}); return e },
		"zero share": func() error { _, e := Compute("shares", 10, []Part{{"a", 0}, {"b", 1}}); return e },
	} {
		if f() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Whatever the inputs, shares add up to the total exactly (the core guarantee), including near the int64 limits.
func TestSharesAlwaysSumToTheTotal(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 20000; i++ {
		total := rng.Int63n(1_000_000_000_000_000) + 1
		if i%3 == 0 {
			total = rng.Int63n(1000) + 1
		}
		n := rng.Intn(12) + 1
		var eq, sh []Part
		var bp []Part
		left := int64(10000)
		for k := 0; k < n; k++ {
			id := string(rune('a' + k))
			eq = append(eq, Part{id, 0})
			sh = append(sh, Part{id, rng.Int63n(1000) + 1})
			v := int64(0)
			if k == n-1 {
				v = left
			} else if left > 0 {
				v = rng.Int63n(left + 1)
			}
			left -= v
			bp = append(bp, Part{id, v})
		}
		for method, parts := range map[string][]Part{"equal": eq, "shares": sh, "percent": bp} {
			s, err := Compute(method, total, parts)
			if err != nil || sum(s) != total {
				t.Fatalf("%s total=%d n=%d: sum=%d err=%v", method, total, n, sum(s), err)
			}
			for _, x := range s {
				if x.Amount < 0 {
					t.Fatalf("negative share %+v", x)
				}
			}
		}
	}
}

func TestSimplifyFindsTheFewestPayments(t *testing.T) {
	// A pays 100 for 4 people. Everyone else owes 25: three payments, all to A.
	p, err := Simplify(map[string]int64{"a": 7500, "b": -2500, "c": -2500, "d": -2500})
	if err != nil || len(p) != 3 {
		t.Fatalf("%v %v", p, err)
	}
	// Two independent pairs settle with two payments, not three (a naive greedy over everyone could use more).
	p, _ = Simplify(map[string]int64{"a": 1000, "b": -1000, "c": 2000, "d": -2000})
	if len(p) != 2 {
		t.Fatalf("two circles need two payments, got %v", p)
	}
	// Six people with balances fall into two independent zero-sum circles ({a,f} and {b,c,d,e}), so the fewest
	// payments is 6 - 2 = 4. (g owes nothing and is ignored.)
	net := map[string]int64{"a": 5, "b": -5, "c": 3, "d": 4, "e": -2, "f": -5, "g": 0}
	p, _ = Simplify(net)
	if len(p) != 4 {
		t.Fatalf("expected 4 payments (6 people in 2 circles), got %d: %v", len(p), p)
	}
	check := map[string]int64{}
	for k, v := range net {
		check[k] = v
	}
	for _, x := range p {
		check[x.From] += x.Amount
		check[x.To] -= x.Amount
		if x.Amount <= 0 {
			t.Fatal("non-positive payment")
		}
	}
	for k, v := range check {
		if v != 0 {
			t.Fatalf("%s left with %d", k, v)
		}
	}
	if _, err := Simplify(map[string]int64{"a": 5, "b": -4}); err == nil {
		t.Fatal("unbalanced input accepted")
	}
	if p, _ := Simplify(map[string]int64{"a": 0, "b": 0}); len(p) != 0 {
		t.Fatal("settled group needs no payments")
	}
}

// Every random balanced group settles to exactly zero, never needs more than n-1 payments, and the result is
// deterministic.
func TestSimplifySettlesEveryoneAndIsDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 400; i++ {
		n := rng.Intn(16) + 2
		net := map[string]int64{}
		var s int64
		for k := 0; k < n-1; k++ {
			v := rng.Int63n(20001) - 10000
			net[string(rune('a'+k))] = v
			s += v
		}
		net[string(rune('a'+n-1))] = -s
		p1, err := Simplify(net)
		if err != nil {
			t.Fatal(err)
		}
		p2, _ := Simplify(net)
		if !reflect.DeepEqual(p1, p2) {
			t.Fatal("not deterministic")
		}
		left := map[string]int64{}
		for k, v := range net {
			left[k] = v
		}
		for _, x := range p1 {
			left[x.From] += x.Amount
			left[x.To] -= x.Amount
		}
		for k, v := range left {
			if v != 0 {
				t.Fatalf("iteration %d: %s left with %d", i, k, v)
			}
		}
		if len(p1) > n-1 {
			t.Fatalf("%d payments for %d people", len(p1), n)
		}
	}
}

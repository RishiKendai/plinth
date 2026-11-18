package groups

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
)

// Part is one participant's input to a split: an exact amount (minor units), basis points (percent x 100), or a
// share weight, depending on the method. Member is any stable identifier; it is also the deterministic tie-breaker.
type Part struct {
	Member string
	Value  int64
}

// Share is what one participant owes. Shares always sum to exactly the total.
type Share struct {
	Member string
	Amount int64
	Input  int64
}

var (
	ErrNoParticipants = errors.New("choose at least one person to split with")
	ErrBadMethod      = errors.New("choose equal, exact, percent or shares")
)

// Compute splits total (minor units, > 0) between the parts. Rounding is deterministic and never loses or invents a
// minor unit: amounts are floored, then the leftover units go one each to the largest fractional remainders, with
// ties broken by member identifier (ascending). The same input always gives the same output, on every device.
func Compute(method string, total int64, parts []Part) ([]Share, error) {
	if total <= 0 {
		return nil, errors.New("the amount must be greater than zero")
	}
	if len(parts) == 0 {
		return nil, ErrNoParticipants
	}
	ps := append([]Part(nil), parts...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].Member < ps[j].Member })
	for i := 1; i < len(ps); i++ {
		if ps[i].Member == ps[i-1].Member {
			return nil, errors.New("a person is listed twice")
		}
	}
	out := make([]Share, len(ps))
	for i, p := range ps {
		out[i] = Share{Member: p.Member, Input: p.Value}
	}
	switch method {
	case "equal":
		n := int64(len(ps))
		base, rem := total/n, total%n
		for i := range out {
			out[i].Amount, out[i].Input = base, 0
			if int64(i) < rem {
				out[i].Amount++
			}
		}
	case "exact":
		var sum int64
		for i, p := range ps {
			if p.Value < 0 {
				return nil, errors.New("amounts cannot be negative")
			}
			out[i].Amount = p.Value
			sum += p.Value
		}
		if sum != total {
			return nil, fmt.Errorf("the amounts add up to %d but the total is %d", sum, total)
		}
	case "percent":
		var sum int64
		for _, p := range ps {
			if p.Value < 0 {
				return nil, errors.New("percentages cannot be negative")
			}
			sum += p.Value
		}
		if sum != 10000 {
			return nil, errors.New("the percentages must add up to 100")
		}
		weighted(total, ps, 10000, out)
	case "shares":
		var sum int64
		for _, p := range ps {
			if p.Value <= 0 {
				return nil, errors.New("each share must be at least 1")
			}
			sum += p.Value
		}
		weighted(total, ps, sum, out)
	default:
		return nil, ErrBadMethod
	}
	var check int64
	for _, s := range out {
		check += s.Amount
	}
	if check != total { // unreachable by construction; a hard stop rather than a silent wrong number
		return nil, fmt.Errorf("internal error: shares sum to %d, not %d", check, total)
	}
	return out, nil
}

// weighted distributes total in proportion to weights (summing to denom) with largest-remainder rounding.
func weighted(total int64, ps []Part, denom int64, out []Share) {
	type rem struct {
		i int
		r *big.Int
	}
	rems := make([]rem, len(ps))
	var used int64
	for i, p := range ps {
		num := new(big.Int).Mul(big.NewInt(total), big.NewInt(p.Value)) // up to 1e15 x 1e15: needs big ints
		q, r := new(big.Int).QuoRem(num, big.NewInt(denom), new(big.Int))
		out[i].Amount = q.Int64()
		used += out[i].Amount
		rems[i] = rem{i, r}
	}
	left := total - used
	sort.SliceStable(rems, func(a, b int) bool {
		if c := rems[a].r.Cmp(rems[b].r); c != 0 {
			return c > 0
		}
		return ps[rems[a].i].Member < ps[rems[b].i].Member
	})
	for k := int64(0); k < left; k++ {
		out[rems[k].i].Amount++
	}
}

// Repayment is a suggested payment from one member to another.
type Repayment struct {
	From, To string
	Amount   int64
}

// maxExact is the largest number of people with a non-zero balance for which the minimum number of payments is
// computed exactly (a subset search that is exponential in this number). Larger groups use a greedy match, which
// still needs at most one payment fewer than the number of people.
const maxExact = 14

// Simplify turns net balances (positive: the group owes them; negative: they owe the group; the total is zero) into
// a short list of payments that settles everyone. For groups up to maxExact people with a balance it finds the
// fewest possible payments: the people split into the largest number of independent zero-sum circles, and each
// circle needs one payment fewer than it has people.
func Simplify(net map[string]int64) ([]Repayment, error) {
	var ids []string
	var sum int64
	for m, v := range net {
		sum += v
		if v != 0 {
			ids = append(ids, m)
		}
	}
	if sum != 0 {
		return nil, errors.New("balances do not add up to zero")
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > maxExact {
		return greedy(ids, net), nil
	}
	n := len(ids)
	size := 1 << n
	total := make([]int64, size)
	for mask := 1; mask < size; mask++ {
		low := mask & -mask
		bit := 0
		for 1<<bit != low {
			bit++
		}
		total[mask] = total[mask^low] + net[ids[bit]]
	}
	// best[mask] = most zero-sum groups the people in mask can be split into; choice[mask] = the group taken.
	best := make([]int, size)
	choice := make([]int, size)
	for mask := 1; mask < size; mask++ {
		if total[mask] != 0 {
			best[mask] = -1
			continue
		}
		low := mask & -mask
		rest := mask ^ low
		best[mask] = -1
		for sub := rest; ; sub = (sub - 1) & rest {
			grp := sub | low
			if total[grp] == 0 && best[mask^grp] >= 0 && best[mask^grp]+1 > best[mask] {
				best[mask], choice[mask] = best[mask^grp]+1, grp
			}
			if sub == 0 {
				break
			}
		}
	}
	var out []Repayment
	for mask := size - 1; mask > 0; {
		grp := choice[mask]
		var members []string
		for b := 0; b < n; b++ {
			if grp&(1<<b) != 0 {
				members = append(members, ids[b])
			}
		}
		out = append(out, greedy(members, net)...)
		mask ^= grp
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out, nil
}

// greedy pays the biggest debtor to the biggest creditor repeatedly. Ties break by identifier for determinism.
func greedy(members []string, net map[string]int64) []Repayment {
	type pe struct {
		id  string
		amt int64
	}
	var cred, debt []pe
	for _, m := range members {
		if net[m] > 0 {
			cred = append(cred, pe{m, net[m]})
		} else if net[m] < 0 {
			debt = append(debt, pe{m, -net[m]})
		}
	}
	less := func(s []pe) func(i, j int) bool {
		return func(i, j int) bool {
			if s[i].amt != s[j].amt {
				return s[i].amt > s[j].amt
			}
			return s[i].id < s[j].id
		}
	}
	var out []Repayment
	for len(cred) > 0 && len(debt) > 0 {
		sort.Slice(cred, less(cred))
		sort.Slice(debt, less(debt))
		pay := cred[0].amt
		if debt[0].amt < pay {
			pay = debt[0].amt
		}
		out = append(out, Repayment{From: debt[0].id, To: cred[0].id, Amount: pay})
		cred[0].amt -= pay
		debt[0].amt -= pay
		if cred[0].amt == 0 {
			cred = cred[1:]
		}
		if debt[0].amt == 0 {
			debt = debt[1:]
		}
	}
	return out
}

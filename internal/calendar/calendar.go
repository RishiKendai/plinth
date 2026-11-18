// Package calendar holds the date arithmetic behind recurring schedules, reminders and budget periods.
//
// All "dates" are civil dates (year, month, day) carried as time.Time at 00:00 UTC; they have no zone. A zone only
// enters when a civil date and a local time are turned into an instant (DueAt), which is how schedules are
// evaluated in their own IANA time zone (CLAUDE.md 5.4).
package calendar

import (
	"fmt"
	"time"
)

type Freq string

const (
	Daily     Freq = "daily"
	Weekly    Freq = "weekly"
	Monthly   Freq = "monthly"
	Quarterly Freq = "quarterly"
	Yearly    Freq = "yearly"
)

func ValidFreq(f string) bool {
	switch Freq(f) {
	case Daily, Weekly, Monthly, Quarterly, Yearly:
		return true
	}
	return false
}

// Date builds a civil date.
func Date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// ParseDate reads YYYY-MM-DD and rejects dates that do not exist (31/02).
func ParseDate(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("not a date: %q", s)
	}
	return t, nil
}

func Format(d time.Time) string { return d.Format("2006-01-02") }

// Today returns the current civil date in loc.
func Today(now time.Time, loc *time.Location) time.Time {
	y, m, d := now.In(loc).Date()
	return Date(y, m, d)
}

func daysIn(y int, m time.Month) int { return Date(y, m+1, 0).Day() }

// addMonths moves by whole months keeping the anchor's day-of-month, clamped to the last day of a shorter month.
// Because every occurrence is computed from the original anchor (never from the previous occurrence), a
// monthly schedule on the 31st goes Jan 31, Feb 28, Mar 31, Apr 30 and never drifts to the 28th.
func addMonths(anchor time.Time, months int) time.Time {
	total := int(anchor.Month()) - 1 + months
	y := anchor.Year() + floorDiv(total, 12)
	m := time.Month(floorMod(total, 12) + 1)
	d := anchor.Day()
	if dim := daysIn(y, m); d > dim {
		d = dim
	}
	return Date(y, m, d)
}

func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
func floorMod(a, b int) int { return a - floorDiv(a, b)*b }

// Nth returns the n-th (0-based) occurrence of "every interval freq" anchored at start.
func Nth(start time.Time, f Freq, interval, n int) time.Time {
	k := n * interval
	switch f {
	case Daily:
		return start.AddDate(0, 0, k)
	case Weekly:
		return start.AddDate(0, 0, 7*k)
	case Monthly:
		return addMonths(start, k)
	case Quarterly:
		return addMonths(start, 3*k)
	default: // Yearly
		return addMonths(start, 12*k)
	}
}

// firstIndexAfter returns the smallest n whose occurrence is strictly after `after`.
func firstIndexAfter(start time.Time, f Freq, interval int, after time.Time) int {
	if !after.After(start.AddDate(0, 0, -1)) { // after < start: the very first occurrence qualifies
		return 0
	}
	var est int
	switch f {
	case Daily:
		est = int(after.Sub(start).Hours()/24) / interval
	case Weekly:
		est = int(after.Sub(start).Hours()/24) / (7 * interval)
	default:
		months := (after.Year()-start.Year())*12 + int(after.Month()) - int(start.Month())
		step := map[Freq]int{Monthly: 1, Quarterly: 3, Yearly: 12}[f] * interval
		est = months / step
	}
	n := est - 2
	if n < 0 {
		n = 0
	}
	for !Nth(start, f, interval, n).After(after) {
		n++
	}
	return n
}

// NextAfter returns the first occurrence strictly after `after`.
func NextAfter(start time.Time, f Freq, interval int, after time.Time) time.Time {
	return Nth(start, f, interval, firstIndexAfter(start, f, interval, after))
}

// Between lists occurrences d with after < d <= through (and d <= end when end is set), oldest first, at most
// limit of them. It never returns the same date twice, so repeated calls with the stored cursor are idempotent.
func Between(start time.Time, f Freq, interval int, after, through time.Time, end *time.Time, limit int) []time.Time {
	var out []time.Time
	for n := firstIndexAfter(start, f, interval, after); len(out) < limit; n++ {
		d := Nth(start, f, interval, n)
		if d.After(through) || (end != nil && d.After(*end)) {
			break
		}
		out = append(out, d)
	}
	return out
}

// DueAt turns a civil date and a local "15:04" time into an instant in zone loc. A local time that does not exist
// (the hour skipped when clocks go forward) resolves to the same wall-clock time using the offset in force after
// the change, so 02:30 on a spring-forward day becomes 03:30 local. An ambiguous time (clocks go back) takes the
// first occurrence.
func DueAt(date time.Time, timeOfDay string, loc *time.Location) (time.Time, error) {
	tod, err := time.Parse("15:04", timeOfDay)
	if err != nil {
		return time.Time{}, fmt.Errorf("not a time of day: %q", timeOfDay)
	}
	t := time.Date(date.Year(), date.Month(), date.Day(), tod.Hour(), tod.Minute(), 0, 0, loc)
	if t.Hour() != tod.Hour() || t.Minute() != tod.Minute() { // fell in a gap: Go moved it; make it deterministic
		return time.Date(date.Year(), date.Month(), date.Day(), tod.Hour()+1, tod.Minute(), 0, 0, loc).UTC(), nil
	}
	return t.UTC(), nil
}

// ---- budget periods ----

// PeriodStart returns the first day of the calendar-aligned period containing d.
// Weeks run Monday to Sunday.
func PeriodStart(period string, d time.Time) time.Time {
	switch period {
	case "weekly":
		off := (int(d.Weekday()) + 6) % 7 // Monday = 0
		return d.AddDate(0, 0, -off)
	case "monthly":
		return Date(d.Year(), d.Month(), 1)
	case "quarterly":
		return Date(d.Year(), time.Month((int(d.Month())-1)/3*3+1), 1)
	default: // yearly
		return Date(d.Year(), time.January, 1)
	}
}

// PeriodEnd returns the last day (inclusive) of the period that begins at start.
func PeriodEnd(period string, start time.Time) time.Time {
	switch period {
	case "weekly":
		return start.AddDate(0, 0, 6)
	case "monthly":
		return Date(start.Year(), start.Month()+1, 0)
	case "quarterly":
		return Date(start.Year(), start.Month()+3, 0)
	default:
		return Date(start.Year(), time.December, 31)
	}
}

// PrevPeriodStart returns the start of the period before the one beginning at start.
func PrevPeriodStart(period string, start time.Time) time.Time {
	return PeriodStart(period, start.AddDate(0, 0, -1))
}

// NextPeriodStart returns the start of the period after the one beginning at start.
func NextPeriodStart(period string, start time.Time) time.Time {
	return PeriodEnd(period, start).AddDate(0, 0, 1)
}

package calendar

import (
	"testing"
	"time"
)

func d(s string) time.Time {
	t, err := ParseDate(s)
	if err != nil {
		panic(err)
	}
	return t
}

func fmtAll(ds []time.Time) []string {
	out := []string{}
	for _, x := range ds {
		out = append(out, Format(x))
	}
	return out
}

func eq(t *testing.T, got []time.Time, want ...string) {
	t.Helper()
	g := fmtAll(got)
	if len(g) != len(want) {
		t.Fatalf("got %v want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("got %v want %v", g, want)
		}
	}
}

func TestMonthEndDoesNotDrift(t *testing.T) {
	// A monthly schedule anchored on the 31st: clamp in short months, snap back to the 31st afterwards.
	got := Between(d("2026-01-31"), Monthly, 1, d("2025-12-31"), d("2026-06-30"), nil, 20)
	eq(t, got, "2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30", "2026-05-31", "2026-06-30")
	// Leap year February.
	eq(t, Between(d("2028-01-31"), Monthly, 1, d("2028-01-31"), d("2028-03-01"), nil, 5), "2028-02-29")
	// Anchored on the 29th: Feb 29 only in leap years.
	eq(t, Between(d("2027-01-29"), Monthly, 1, d("2027-01-29"), d("2027-03-31"), nil, 5), "2027-02-28", "2027-03-29")
}

func TestYearlyLeapDay(t *testing.T) {
	got := Between(d("2028-02-29"), Yearly, 1, d("2027-12-31"), d("2032-12-31"), nil, 10)
	eq(t, got, "2028-02-29", "2029-02-28", "2030-02-28", "2031-02-28", "2032-02-29")
}

func TestQuarterlyAndCustomIntervals(t *testing.T) {
	eq(t, Between(d("2026-11-30"), Quarterly, 1, d("2026-01-01"), d("2027-12-31"), nil, 10),
		"2026-11-30", "2027-02-28", "2027-05-30", "2027-08-30", "2027-11-30")
	eq(t, Between(d("2026-11-18"), Weekly, 2, d("2026-11-17"), d("2027-01-01"), nil, 10),
		"2026-11-18", "2026-12-02", "2026-12-16", "2026-12-30")
	eq(t, Between(d("2026-01-15"), Monthly, 6, d("2026-01-01"), d("2028-01-01"), nil, 10), "2026-01-15", "2026-07-15", "2027-01-15", "2027-07-15")
	eq(t, Between(d("2026-11-18"), Daily, 10, d("2026-11-17"), d("2026-12-20"), nil, 10), "2026-11-18", "2026-11-28", "2026-12-08", "2026-12-18")
}

func TestEndDateAndCursorAreIdempotent(t *testing.T) {
	end := d("2026-03-15")
	first := Between(d("2026-01-10"), Monthly, 1, d("2026-01-09"), d("2026-12-31"), &end, 50)
	eq(t, first, "2026-01-10", "2026-02-10", "2026-03-10")
	// Calling again from the stored cursor yields nothing: no duplicates, ever.
	if again := Between(d("2026-01-10"), Monthly, 1, first[len(first)-1], d("2026-12-31"), &end, 50); len(again) != 0 {
		t.Fatalf("expected none, got %v", fmtAll(again))
	}
	// Splitting a range in two gives the same dates as one call.
	whole := Between(d("2026-01-31"), Monthly, 1, d("2026-01-01"), d("2026-12-31"), nil, 50)
	a := Between(d("2026-01-31"), Monthly, 1, d("2026-01-01"), d("2026-06-30"), nil, 50)
	b := Between(d("2026-01-31"), Monthly, 1, d("2026-06-30"), d("2026-12-31"), nil, 50)
	if len(whole) != len(a)+len(b) {
		t.Fatalf("split mismatch %d vs %d+%d", len(whole), len(a), len(b))
	}
}

func TestBetweenStartsFarFromAnchorQuickly(t *testing.T) {
	// Ten years after the anchor: must jump, not walk 3,650 steps (and must still be right).
	got := Between(d("2016-01-31"), Daily, 1, d("2026-11-16"), d("2026-11-18"), nil, 10)
	eq(t, got, "2026-11-17", "2026-11-18")
	got = Between(d("2016-01-31"), Monthly, 1, d("2026-10-31"), d("2026-12-31"), nil, 10)
	eq(t, got, "2026-11-30", "2026-12-31")
}

func TestDueAtTimeZoneAndDST(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	ist, _ := time.LoadLocation("Asia/Kolkata")
	got, _ := DueAt(d("2026-11-18"), "09:00", ist)
	if got.Format(time.RFC3339) != "2026-11-18T03:30:00Z" {
		t.Fatalf("IST 09:00 = %s", got.Format(time.RFC3339))
	}
	// Same local 09:00 on either side of the November clock change: the UTC instant moves by an hour.
	before, _ := DueAt(d("2026-10-31"), "09:00", ny)
	after, _ := DueAt(d("2026-11-02"), "09:00", ny)
	if before.Format(time.RFC3339) != "2026-10-31T13:00:00Z" || after.Format(time.RFC3339) != "2026-11-02T14:00:00Z" {
		t.Fatalf("DST shift: %s %s", before.Format(time.RFC3339), after.Format(time.RFC3339))
	}
	// 02:30 does not exist on 2026-03-08 in New York: resolves to 03:30 local (07:30 UTC), deterministically.
	gap, _ := DueAt(d("2026-03-08"), "02:30", ny)
	if gap.Format(time.RFC3339) != "2026-03-08T07:30:00Z" {
		t.Fatalf("gap time = %s", gap.Format(time.RFC3339))
	}
	// 01:30 on 2026-11-01 happens twice: the first (daylight) one is used.
	amb, _ := DueAt(d("2026-11-01"), "01:30", ny)
	if amb.Format(time.RFC3339) != "2026-11-01T05:30:00Z" {
		t.Fatalf("ambiguous time = %s", amb.Format(time.RFC3339))
	}
	if _, err := DueAt(d("2026-11-18"), "25:00", ist); err == nil {
		t.Fatal("bad time of day accepted")
	}
}

func TestTodayUsesTheZone(t *testing.T) {
	now := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC) // 01:30 on the 19th in Kolkata
	ist, _ := time.LoadLocation("Asia/Kolkata")
	if got := Format(Today(now, ist)); got != "2026-11-19" {
		t.Fatalf("got %s", got)
	}
	if got := Format(Today(now, time.UTC)); got != "2026-11-18" {
		t.Fatalf("got %s", got)
	}
}

func TestPeriods(t *testing.T) {
	cases := []struct{ period, day, start, end string }{
		{"weekly", "2026-11-18", "2026-11-16", "2026-11-22"}, // Wednesday -> Mon..Sun
		{"weekly", "2026-11-22", "2026-11-16", "2026-11-22"}, // Sunday belongs to the week that began Monday
		{"weekly", "2026-11-16", "2026-11-16", "2026-11-22"},
		{"monthly", "2026-02-10", "2026-02-01", "2026-02-28"},
		{"monthly", "2028-02-10", "2028-02-01", "2028-02-29"},
		{"quarterly", "2026-11-18", "2026-10-01", "2026-12-31"},
		{"quarterly", "2026-03-31", "2026-01-01", "2026-03-31"},
		{"yearly", "2026-11-18", "2026-01-01", "2026-12-31"},
	}
	for _, c := range cases {
		s := PeriodStart(c.period, d(c.day))
		if Format(s) != c.start || Format(PeriodEnd(c.period, s)) != c.end {
			t.Errorf("%s %s: got %s..%s want %s..%s", c.period, c.day, Format(s), Format(PeriodEnd(c.period, s)), c.start, c.end)
		}
	}
	if got := Format(PrevPeriodStart("monthly", d("2026-01-01"))); got != "2025-12-01" {
		t.Fatalf("prev %s", got)
	}
	if got := Format(NextPeriodStart("quarterly", d("2026-10-01"))); got != "2027-01-01" {
		t.Fatalf("next %s", got)
	}
}

func TestParseDateRejectsImpossibleDates(t *testing.T) {
	for _, s := range []string{"2026-02-30", "2026-13-01", "26-01-01", "", "2026-1-1x"} {
		if _, err := ParseDate(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

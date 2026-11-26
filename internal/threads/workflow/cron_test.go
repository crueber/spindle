package workflow

import (
	"testing"
	"time"
)

// utc parses a fixture timestamp (UTC, minute precision).
func utc(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02T15:04:05Z", s)
	if err != nil {
		t.Fatalf("fixture time %q: %v", s, err)
	}
	return ts
}

// TestCronTickMath pins the documented subset: `*`, numbers, steps, ranges and
// lists, evaluated in UTC with no DST (a UTC clock has none by construction).
func TestCronTickMath(t *testing.T) {
	cases := []struct {
		expr string
		prev string // the tick at or before the probe
		next string // the first tick after the probe
		at   string
	}{
		{"*/15 * * * *", "2026-11-21T05:45:00Z", "2026-11-21T06:00:00Z", "2026-11-21T05:47:00Z"},
		{"0 3 * * *", "2026-11-21T03:00:00Z", "2026-11-22T03:00:00Z", "2026-11-21T12:00:00Z"},
		{"0 3 * * *", "2026-11-21T03:00:00Z", "2026-11-22T03:00:00Z", "2026-11-21T03:00:00Z"},
		{"30 9 * * 1-5", "2026-11-20T09:30:00Z", "2026-11-23T09:30:00Z", "2026-11-21T12:00:00Z"},
		{"0 0 1 * *", "2026-11-01T00:00:00Z", "2026-12-01T00:00:00Z", "2026-11-21T12:00:00Z"},
		{"5,35 * * * *", "2026-11-21T05:35:00Z", "2026-11-21T06:05:00Z", "2026-11-21T05:47:00Z"},
		{"0 */6 * * *", "2026-11-21T06:00:00Z", "2026-11-21T12:00:00Z", "2026-11-21T09:10:00Z"},
	}
	for _, tc := range cases {
		c, err := ParseCron(tc.expr)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		at := utc(t, tc.at)
		next, ok := c.Next(at)
		if !ok || !next.Equal(utc(t, tc.next)) {
			t.Errorf("%s: Next(%s) = %s (%v), want %s", tc.expr, tc.at, next.Format(time.RFC3339), ok, tc.next)
		}
		prev, ok := c.Prev(at)
		if !ok || !prev.Equal(utc(t, tc.prev)) {
			t.Errorf("%s: Prev(%s) = %s (%v), want %s", tc.expr, tc.at, prev.Format(time.RFC3339), ok, tc.prev)
		}
	}
}

// TestCronDayOfMonthVsWeekday pins the Vixie OR rule.
func TestCronDayOfMonthVsWeekday(t *testing.T) {
	c, err := ParseCron("0 0 1 * 1") // the 1st OR every Monday
	if err != nil {
		t.Fatal(err)
	}
	// 2026-11-02 is a Monday; 2026-11-01 is a Sunday and the 1st.
	for _, day := range []string{"2026-11-01T00:00:00Z", "2026-11-02T00:00:00Z"} {
		hit, ok := c.Prev(utc(t, day))
		if !ok || !hit.Equal(utc(t, day)) {
			t.Fatalf("Prev(%s) = %v, want the day itself (dom OR dow)", day, hit)
		}
	}
	// A `*` day-of-week defers to day-of-month alone.
	c, err = ParseCron("0 0 15 * *")
	if err != nil {
		t.Fatal(err)
	}
	if hit, _ := c.Prev(utc(t, "2026-11-14T23:59:00Z")); hit.Format("2006-01-02") != "2026-10-15" {
		t.Fatalf("monthly tick = %v, want 2026-10-15", hit)
	}
}

// TestCronStepAndRangeEdge covers the parsing edges the scheduler relies on.
func TestCronStepAndRangeEdge(t *testing.T) {
	// Sunday spelled 7 is Sunday spelled 0.
	a, err := ParseCron("0 0 * * 7")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseCron("0 0 * * 0")
	if err != nil {
		t.Fatal(err)
	}
	probe := utc(t, "2026-11-21T12:00:00Z")
	hitA, _ := a.Prev(probe)
	hitB, _ := b.Prev(probe)
	if !hitA.Equal(hitB) || hitA.Weekday() != time.Sunday {
		t.Fatalf("7 != 0: %v vs %v", hitA, hitB)
	}
	// `5/15` means "from 5 to the field max, every 15".
	c, err := ParseCron("5/15 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"05:05", "05:20", "05:35", "05:50", "06:05"}
	cur := utc(t, "2026-11-21T05:04:00Z")
	for _, w := range want {
		next, ok := c.Next(cur)
		if !ok || next.Format("15:04") != w {
			t.Fatalf("step ticks: got %v, want %s", next, w)
		}
		cur = next
	}
	// An impossible date has no tick (never a runaway loop).
	if _, ok := mustCron(t, "0 0 31 2 *").Next(probe); ok {
		t.Fatal("February 31 produced a tick")
	}
	if _, ok := mustCron(t, "0 0 31 2 *").Prev(probe); ok {
		t.Fatal("February 31 produced a tick")
	}
}

func mustCron(t *testing.T, expr string) *Cron {
	t.Helper()
	c, err := ParseCron(expr)
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return c
}

// TestCronRejectsUnsupportedSyntax pins the per-file error contract: everything
// outside the documented subset is a parse error, never an ignored field.
func TestCronRejectsUnsupportedSyntax(t *testing.T) {
	bad := []string{
		"",
		"* * * *",
		"* * * * * *",
		"@every 5m",
		"0 3 * * FUNDAY",
		"*/0 * * * *",
		"60 * * * *",
		"0 24 * * *",
		"0 0 0 * *",
		"0 0 * 13 *",
		"0 0 * * 8",
		"5-1 * * * *",
		"1,,2 * * * *",
		"a * * * *",
		"L 0 * * *",
		"0 0 * * 5#9",
	}
	for _, expr := range bad {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("accepted unsupported cron %q", expr)
		}
	}
	if _, err := ParseCron(" 0  3 * * * "); err != nil {
		t.Errorf("whitespace-padded cron rejected: %v", err)
	}
}

// TestScheduleEntriesValidatedAtParse pins that an unsupported cron is that
// file's parse error (the workflow parser's per-file isolation).
func TestScheduleEntriesValidatedAtParse(t *testing.T) {
	if _, err := Parse([]byte("on:\n  schedule:\n    - cron: \"*/5 * * * *\"\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err != nil {
		t.Fatalf("valid cron rejected: %v", err)
	}
	if _, err := Parse([]byte("on:\n  schedule: []\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("empty schedule accepted")
	}
	if _, err := Parse([]byte("on:\n  schedule:\n    - cron: \"0 3 * * FUNDAY\"\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("unknown weekday accepted")
	}
}

// TestCronStringAndNil pins the display/nil arms.
func TestCronStringAndNil(t *testing.T) {
	if got := (*Cron)(nil).String(); got != "" {
		t.Fatalf("nil String = %q", got)
	}
	c := mustCron(t, " 0 3 * * * ")
	if got := c.String(); got != "0 3 * * *" {
		t.Fatalf("String = %q", got)
	}
	if _, ok := (*Cron)(nil).Next(utc(t, "2026-11-21T00:00:00Z")); ok {
		t.Fatal("nil Next returned a tick")
	}
	if _, ok := (*Cron)(nil).Prev(utc(t, "2026-11-21T00:00:00Z")); ok {
		t.Fatal("nil Prev returned a tick")
	}
}

// TestCronListAndStepArms pins the field-parser's list/step edges.
func TestCronListAndStepArms(t *testing.T) {
	bad := []string{
		"1,,2 * * * *",  // empty list element
		"*/x * * * *",   // non-numeric step
		"1-5/2 * * * *", // fine, checked below
	}
	for _, expr := range bad[:2] {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("accepted %q", expr)
		}
	}
	c := mustCron(t, "1-5/2 * * * *")
	if hit, ok := c.Next(utc(t, "2026-11-21T05:00:00Z")); !ok || hit.Minute() != 1 {
		t.Fatalf("range step tick = %v", hit)
	}
}

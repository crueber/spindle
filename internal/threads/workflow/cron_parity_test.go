package workflow

import (
	"testing"
	"time"
)

// TestCronMacrosAndNames pins the @daily family and month/weekday names
// (PAR-022): both are the documented spelling, not an unsupported extension.
func TestCronMacrosAndNames(t *testing.T) {
	macros := map[string]string{
		"@yearly":   "0 0 1 1 *",
		"@annually": "0 0 1 1 *",
		"@monthly":  "0 0 1 * *",
		"@weekly":   "0 0 * * 0",
		"@daily":    "0 0 * * *",
		"@midnight": "0 0 * * *",
		"@hourly":   "0 * * * *",
	}
	probe := utc(t, "2026-11-21T12:34:00Z")
	for macro, expr := range macros {
		a, err := ParseCron(macro)
		if err != nil {
			t.Fatalf("%s: %v", macro, err)
		}
		b, err := ParseCron(expr)
		if err != nil {
			t.Fatal(err)
		}
		pa, oka := a.Prev(probe)
		pb, okb := b.Prev(probe)
		if oka != okb || !pa.Equal(pb) {
			t.Errorf("%s = %v, want %v", macro, pa, pb)
		}
	}
	// `0 3 * * MON-FRI` is the same as its numeric spelling.
	named, err := ParseCron("0 3 * * MON-FRI")
	if err != nil {
		t.Fatal(err)
	}
	numeric, _ := ParseCron("0 3 * * 1-5")
	a, _ := named.Prev(probe)
	b, _ := numeric.Prev(probe)
	if !a.Equal(b) {
		t.Fatalf("MON-FRI = %v, want %v", a, b)
	}
	// A month name works in the month field.
	if _, err := ParseCron("0 0 1 JAN *"); err != nil {
		t.Fatalf("month name: %v", err)
	}
}

// TestCronLastNearestAndNth pins `L`, `L-N`, `NW`, `DL` and `D#n`.
func TestCronLastNearestAndNth(t *testing.T) {
	// The last day of November 2026 is the 30th.
	last, err := ParseCron("0 0 L * *")
	if err != nil {
		t.Fatal(err)
	}
	if hit, ok := last.Prev(utc(t, "2026-11-30T00:00:00Z")); !ok || hit.Format("2006-01-02") != "2026-11-30" {
		t.Fatalf("L = %v", hit)
	}
	if hit, ok := last.Prev(utc(t, "2026-11-29T23:59:00Z")); !ok || hit.Format("2006-01-02") != "2026-10-31" {
		t.Fatalf("L (previous month) = %v", hit)
	}
	// L-3: 2026-11-27.
	l3, err := ParseCron("0 0 L-3 * *")
	if err != nil {
		t.Fatal(err)
	}
	if hit, _ := l3.Prev(utc(t, "2026-11-30T10:00:00Z")); hit.Format("2006-01-02") != "2026-11-27" {
		t.Fatalf("L-3 = %v", hit)
	}
	// 15W in November 2026: the 15th is a Sunday, so Monday the 16th.
	nw, err := ParseCron("0 0 15W * *")
	if err != nil {
		t.Fatal(err)
	}
	if hit, _ := nw.Prev(utc(t, "2026-11-30T10:00:00Z")); hit.Format("2006-01-02") != "2026-11-16" {
		t.Fatalf("15W = %v", hit)
	}
	// 5L: the last Friday of November 2026 is the 27th.
	lastFri, err := ParseCron("0 0 * * 5L")
	if err != nil {
		t.Fatal(err)
	}
	if hit, _ := lastFri.Prev(utc(t, "2026-11-30T10:00:00Z")); hit.Format("2006-01-02") != "2026-11-27" {
		t.Fatalf("5L = %v", hit)
	}
	// 1#2: the second Monday of November 2026 is the 9th.
	nth, err := ParseCron("0 0 * * 1#2")
	if err != nil {
		t.Fatal(err)
	}
	if hit, _ := nth.Prev(utc(t, "2026-11-30T10:00:00Z")); hit.Format("2006-01-02") != "2026-11-09" {
		t.Fatalf("1#2 = %v", hit)
	}
}

// TestCronTimezoneDST pins the documented DST rule: a spring-forward local time
// has no tick; a fall-back repeated local time is one wall-clock slot (the
// scheduler coalesces it to a single fire).
func TestCronTimezoneDST(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	c, err := ParseCron("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-29: 02:30 does not exist in Berlin (clocks jump 02:00 → 03:00).
	springNow := time.Date(2026, 3, 29, 12, 0, 0, 0, berlin)
	hit, ok := c.PrevIn(springNow, berlin)
	if !ok || hit.Format("2006-01-02") != "2026-03-28" {
		t.Fatalf("spring-forward tick = %v (%v), want 2026-03-28", hit, ok)
	}
	// 2026-10-25: 02:30 happens twice; both instants name the same slot.
	first := time.Date(2026, 10, 25, 2, 30, 0, 0, berlin)
	slot := c.Slot(first)
	afterSecond := time.Date(2026, 10, 25, 3, 30, 0, 0, berlin)
	hit2, ok := c.PrevIn(afterSecond, berlin)
	if !ok || c.Slot(hit2) != slot {
		t.Fatalf("fall-back slot = %q (%v), want %q", c.Slot(hit2), ok, slot)
	}
	// A location-aware entry validates; an unknown zone is the file's error.
	loc, err := (CronEntry{Cron: "0 3 * * *", Timezone: "Europe/Berlin"}).Location()
	if err != nil || loc.String() != "Europe/Berlin" {
		t.Fatalf("timezone = %v, %v", loc, err)
	}
	if _, err := (CronEntry{Cron: "0 3 * * *", Timezone: "Mars/Olympus"}).Location(); err == nil {
		t.Fatal("unknown timezone accepted")
	}
}

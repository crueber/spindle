package main

// webui_maintenance_test.go — the composition projection onto the Maintenance
// page's task-kind table (docs/PLAN-ADMIN.md §3.5): the canonical kinds keep
// their order, a kind with no runs stays an honest empty value, follow rounds
// aggregate by outcome, and an unknown future kind is appended, never dropped.

import (
	"testing"

	"github.com/crueber/spindle/internal/maintain"
)

func TestAdminUnitRows(t *testing.T) {
	snap := maintain.MetricsSnapshot{
		Units: map[string]map[maintain.Outcome]int64{
			maintain.KindCheckpoint: {maintain.OutcomeOK: 4, maintain.OutcomeError: 1},
			maintain.KindFsck:       {maintain.OutcomeIdle: 2},
			"future-kind":           {maintain.OutcomeOK: 1},
		},
		FollowRoundsByOutcome: map[string]int64{
			"acme/one ok":    2,
			"acme/two ok":    3,
			"acme/two error": 1,
		},
	}
	rows := adminUnitRows(snap)
	if len(rows) != 8 {
		t.Fatalf("rows = %d, want 8 (7 canonical + 1 future)\n%+v", len(rows), rows)
	}
	order := []string{
		maintain.KindCheckpoint, maintain.KindRepair, maintain.KindBundle,
		maintain.KindCompact, maintain.KindRevIndex, maintain.KindFsck,
		maintain.KindFollow, "future-kind",
	}
	for i, want := range order {
		if rows[i].Key != want {
			t.Fatalf("row %d = %q, want %q (%+v)", i, rows[i].Key, want, rows)
		}
	}
	if rows[0].Value != "error 1 · ok 4" {
		t.Fatalf("checkpoint outcomes = %q", rows[0].Value)
	}
	if rows[1].Value != "" {
		t.Fatalf("a kind with no runs must stay empty, got %q", rows[1].Value)
	}
	if rows[5].Value != "idle 2" {
		t.Fatalf("fsck outcomes = %q", rows[5].Value)
	}
	if rows[6].Value != "error 1 · ok 5" {
		t.Fatalf("follow outcomes = %q", rows[6].Value)
	}
	if rows[7].Value != "ok 1" {
		t.Fatalf("future kind outcomes = %q", rows[7].Value)
	}
	if outcomeSummary(nil) != "" {
		t.Fatal("empty outcomes must render no value")
	}
}

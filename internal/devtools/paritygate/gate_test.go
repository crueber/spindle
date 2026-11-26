// The gate's own rules (PAR-201): a green row must name a test that exists, an
// unplanned gap is red, a non-goal needs its signatory, the plan and the matrix
// cannot disagree on the id set, and a stale matrix is red. Every case mutates a
// copy of the real parity.json, so the tests cannot drift from the shape the tree
// actually ships.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture loads the committed matrix and the committed test index.
func fixture(t *testing.T) (*Doc, ValidateOptions) {
	t.Helper()
	doc, err := Load(filepath.Join("..", "..", "..", "docs", "features", "parity", "parity.json"))
	if err != nil {
		t.Fatalf("load parity.json: %v", err)
	}
	tests, err := ScanTests(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("scan tests: %v", err)
	}
	plan := planIDs(filepath.Join("..", "..", "..", "docs", "features", "21_parity_master.md"))
	if len(plan) == 0 {
		t.Fatal("plan id set is empty")
	}
	return doc, ValidateOptions{Tests: tests, PlanIDs: plan}
}

// row returns one row by id (fails when the fixture loses it).
func row(t *testing.T, doc *Doc, id string) *Row {
	t.Helper()
	for _, r := range doc.Rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("fixture has no %s", id)
	return nil
}

func TestParityMatrixCoversEveryPlanRow(t *testing.T) {
	doc, opt := fixture(t)
	if findings := doc.Validate(opt); len(findings) > 0 {
		t.Fatalf("the committed matrix does not validate: %v", findings)
	}
	// Every plan row is present exactly once, and the reverse holds too.
	seen := map[string]bool{}
	for _, r := range doc.Rows {
		if seen[r.ID] {
			t.Fatalf("%s appears twice", r.ID)
		}
		seen[r.ID] = true
		if !opt.PlanIDs[r.ID] {
			t.Fatalf("%s is not in the plan", r.ID)
		}
	}
	for id := range opt.PlanIDs {
		if !seen[id] {
			t.Fatalf("plan row %s has no matrix row", id)
		}
	}
	if len(doc.Rows) != len(opt.PlanIDs) {
		t.Fatalf("%d rows for %d plan ids", len(doc.Rows), len(opt.PlanIDs))
	}
	if doc.DocsRevision.Forgejo == "" || doc.DocsRevision.GitHub == "" || doc.DocsRevision.Checked == "" {
		t.Fatalf("docs stamp incomplete: %+v", doc.DocsRevision)
	}
}

func TestParityGateRejectsAGreenRowWithoutATest(t *testing.T) {
	doc, opt := fixture(t)
	r := row(t, doc, "PAR-150")
	r.Tests = nil
	findings := doc.Validate(opt)
	if !hasFinding(findings, "PAR-150", "no test") {
		t.Fatalf("a have row with no test passed the gate: %v", findings)
	}
}

func TestParityGateRejectsAMissingNamedTest(t *testing.T) {
	doc, opt := fixture(t)
	r := row(t, doc, "PAR-150")
	r.Tests = append([]string{}, r.Tests...)
	r.Tests[0] = "TestParityThisTestWasDeleted"
	findings := doc.Validate(opt)
	if !hasFinding(findings, "PAR-150", "does not exist") {
		t.Fatalf("a deleted test passed the gate: %v", findings)
	}
	// A path-qualified reference must name the file that declares it.
	r.Tests[0] = "internal/e2e/nope_test.go#TestParityCorpusParsesAndPlans"
	if findings := doc.Validate(opt); !hasFinding(findings, "PAR-150", "lives in") && !hasFinding(findings, "PAR-150", "does not exist") {
		t.Fatalf("a wrong-file reference passed the gate: %v", findings)
	}
}

func TestParityGateRejectsUnplannedRowsAndNonGoals(t *testing.T) {
	doc, opt := fixture(t)
	r := row(t, doc, "PAR-150")
	r.Status = StatusMissing
	r.Wave = 0
	findings := doc.Validate(opt)
	if !hasFinding(findings, "PAR-150", "neither a wave nor a decision") {
		t.Fatalf("an unplanned gap passed the gate: %v", findings)
	}
	r.Status = StatusNonGoal
	r.Wave = 1
	findings = doc.Validate(opt)
	if !hasFinding(findings, "PAR-150", "approved_by") {
		t.Fatalf("a non-goal with no signatory passed the gate: %v", findings)
	}
	r.ApprovedBy = "operator"
	if findings := doc.Validate(opt); len(findings) != 0 {
		t.Fatalf("a signed non-goal stayed red: %v", findings)
	}
	// Closure mode is what rejects a planned missing row.
	r.Status = StatusMissing
	r.ApprovedBy = ""
	closure := opt
	closure.Strict = true
	if findings := doc.Validate(closure); !hasFinding(findings, "PAR-150", "closure mode") {
		t.Fatalf("closure mode accepted a missing row: %v", findings)
	}
}

func TestParityGatePlanIDSetMustMatch(t *testing.T) {
	doc, opt := fixture(t)
	// A row in the matrix the plan does not have.
	doc.Rows = append(doc.Rows, &Row{ID: "PAR-999", Area: "discovery", Capability: "invented",
		Provenance: "none", Status: StatusMissing, Wave: 4, Track: []string{"T8"}})
	if findings := doc.Validate(opt); !hasFinding(findings, "PAR-999", "does not list it") {
		t.Fatalf("an invented id passed the gate: %v", findings)
	}
	// A plan id the matrix lost.
	doc.Rows = doc.Rows[:len(doc.Rows)-1]
	for i, r := range doc.Rows {
		if r.ID == "PAR-001" {
			doc.Rows = append(doc.Rows[:i], doc.Rows[i+1:]...)
			break
		}
	}
	if findings := doc.Validate(opt); !hasFinding(findings, "PAR-001", "absent from parity.json") {
		t.Fatalf("a plan row missing from the matrix passed the gate: %v", findings)
	}
	// Two rows with one id is red.
	doc, opt = fixture(t)
	dup := *row(t, doc, "PAR-150")
	doc.Rows = append(doc.Rows, &dup)
	if findings := doc.Validate(opt); !hasFinding(findings, "PAR-150", "appears twice") {
		t.Fatalf("a duplicate id passed the gate: %v", findings)
	}
}

func TestParityMatrixIsFresh(t *testing.T) {
	doc, opt := fixture(t)
	if findings := doc.Validate(opt); len(findings) > 0 {
		t.Fatalf("fixture invalid: %v", findings)
	}
	rendered := doc.Render()
	onDisk, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "features", "parity", "matrix.md"))
	if err != nil {
		t.Fatalf("read matrix.md: %v", err)
	}
	if string(onDisk) != rendered {
		t.Fatal("docs/features/parity/matrix.md is stale: regenerate with `go run ./internal/devtools/paritygate -check=false`")
	}
	// The render is deterministic and names every area's rows.
	doc.Rows[0].Status = StatusMissing
	if doc.Render() == rendered {
		t.Fatal("the render ignores row state")
	}
	if !strings.Contains(rendered, "forgejo "+doc.DocsRevision.Forgejo) {
		t.Fatal("the render lost the docs stamp")
	}
	if !strings.Contains(rendered, "PAR-150") || !strings.Contains(rendered, "| have |") {
		t.Fatal("the render lost the row table shape")
	}
}

func hasFinding(fs []Finding, id, substr string) bool {
	for _, f := range fs {
		if f.ID == id && strings.Contains(f.Detail, substr) {
			return true
		}
	}
	return false
}

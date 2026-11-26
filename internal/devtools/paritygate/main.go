// Command paritygate is the conformance matrix's gate (PAR-201, PAR-202):
//
//	docs/features/parity/parity.json   the machine source of truth (one row per PAR- id)
//	docs/features/parity/matrix.md     generated from it; never edited by hand
//
// It fails when the matrix and the tree disagree:
//
//   - a row is `have` and names no test, or names a Test… function that does not
//     exist under internal/**;
//   - a row is `missing`/`partial` with neither a wave nor a decision (an
//     unplanned gap);
//   - a row is a `non-goal` without an approved_by;
//   - the master plan's id set and the JSON disagree (a row the plan has and the
//     matrix lost, or a matrix row the plan does not have);
//   - matrix.md differs from a fresh render (a hand-edited or stale matrix).
//
// `-strict` is the CLOSURE rule (docs/features/21_parity_master.md §9): no
// `missing` row at all. It is a separate mode because Wave 1 lands with the
// plan's later waves still `missing` by design — the integrity rules are the CI
// gate, and `make parity-closed` is the check that the plan is finished.
//
// With WEAVE_CARD_URL and WEAVE_TOKEN set, the same comparison also runs against
// the master card body (the ids are mirrored there), so a checklist item cannot
// exist only on the board. Without them it prints a notice and skips.
//
// It is a plain Go program (encoding/json only), so `make parity` needs no new
// dependency and no browser.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	var (
		root    = flag.String("root", ".", "repository root")
		dir     = flag.String("dir", "docs/features/parity", "directory holding parity.json and matrix.md")
		check   = flag.Bool("check", true, "verify matrix.md is a fresh render instead of rewriting it")
		strict  = flag.Bool("strict", false, "closure rule: fail on any `missing` row (§9)")
		plan    = flag.String("plan", "docs/features/21_parity_master.md", "the master plan whose id set must match")
		verbose = flag.Bool("v", false, "print every finding, not just the first per row")
	)
	flag.Parse()

	dirPath := *dir
	if !filepath.IsAbs(dirPath) {
		dirPath = filepath.Join(*root, dirPath)
	}
	planPath := *plan
	if !filepath.IsAbs(planPath) {
		planPath = filepath.Join(*root, planPath)
	}

	doc, err := Load(filepath.Join(dirPath, "parity.json"))
	if err != nil {
		fail(err)
	}
	tests, err := ScanTests(*root)
	if err != nil {
		fail(err)
	}
	findings := doc.Validate(ValidateOptions{
		Tests:   tests,
		PlanIDs: planIDs(planPath),
		Strict:  *strict,
	})
	if *verbose {
		for _, f := range findings {
			fmt.Fprintln(os.Stderr, f)
		}
	}
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintln(os.Stderr, "parity: "+f.String())
		}
		fail(fmt.Errorf("%d parity matrix finding(s)", len(findings)))
	}
	if err := checkWeaveCard(doc); err != nil {
		fail(err)
	}

	rendered := doc.Render()
	matrixPath := filepath.Join(dirPath, "matrix.md")
	if *check {
		onDisk, rerr := os.ReadFile(matrixPath)
		if rerr != nil {
			fail(fmt.Errorf("read %s: %w (run `go run ./internal/devtools/paritygate -check=false`)", matrixPath, rerr))
		}
		if string(onDisk) != rendered {
			fail(fmt.Errorf("%s is stale: regenerate with `go run ./internal/devtools/paritygate -check=false`", matrixPath))
		}
	} else if werr := os.WriteFile(matrixPath, []byte(rendered), 0o644); werr != nil {
		fail(fmt.Errorf("write %s: %w", matrixPath, werr))
	}
	fmt.Print(doc.Summary(*strict))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "paritygate:", err)
	os.Exit(1)
}

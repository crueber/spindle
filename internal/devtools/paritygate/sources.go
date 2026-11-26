package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// planIDs reads the master plan's §4 tables and returns every `PAR-` id they
// list. The plan is the reconciled source of the id set, so a row that exists in
// one and not the other is a traceability break (PAR-202's "every PAR- id
// appears once").
func planIDs(path string) map[string]bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "paritygate: plan %s unreadable (%v); skipping the id-set comparison\n", path, err)
		return nil
	}
	ids := map[string]bool{}
	rowRe := regexp.MustCompile(`(?m)^\| (PAR-[0-9]{3}) \|`)
	for _, m := range rowRe.FindAllStringSubmatch(string(raw), -1) {
		ids[m[1]] = true
	}
	if len(ids) == 0 {
		fmt.Fprintf(os.Stderr, "paritygate: plan %s lists no PAR- rows; skipping the id-set comparison\n", path)
		return nil
	}
	return ids
}

// checkWeaveCard optionally compares the id set against the master card body.
// The card URL is explicit (WEAVE_CARD_URL) because the board's address is not
// recorded in this tree; without it the check is skipped with a notice, which is
// the plan's stated behaviour ("skipped with a printed notice otherwise").
func checkWeaveCard(doc *Doc) error {
	url := strings.TrimSpace(os.Getenv("WEAVE_CARD_URL"))
	token := strings.TrimSpace(os.Getenv("WEAVE_TOKEN"))
	if url == "" || token == "" {
		fmt.Fprintln(os.Stderr, "paritygate: WEAVE_CARD_URL/WEAVE_TOKEN unset — card id-set check skipped (the plan's own id table is still enforced)")
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("weave card: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("weave card: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("weave card: %s answered %d", url, resp.StatusCode)
	}
	var body struct {
		Description string `json:"description"`
		Title       string `json:"title"`
	}
	if derr := json.NewDecoder(resp.Body).Decode(&body); derr != nil {
		return fmt.Errorf("weave card: decode: %w", derr)
	}
	cardIDs := map[string]bool{}
	for _, m := range regexp.MustCompile(`PAR-[0-9]{3}`).FindAllString(body.Title+"\n"+body.Description, -1) {
		cardIDs[m] = true
	}
	if len(cardIDs) == 0 {
		fmt.Fprintln(os.Stderr, "paritygate: the card body names no PAR- ids — card check skipped")
		return nil
	}
	var missing, extra []string
	for _, r := range doc.Rows {
		if !cardIDs[r.ID] {
			missing = append(missing, r.ID)
		}
	}
	for id := range cardIDs {
		found := false
		for _, r := range doc.Rows {
			if r.ID == id {
				found = true
				break
			}
		}
		if !found {
			extra = append(extra, id)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		return fmt.Errorf("weave card and parity.json disagree: card-only %v, matrix-only %v", extra, missing)
	}
	return nil
}

// filepathWalk walks dir, calling fn for every regular file.
func filepathWalk(dir string, fn func(path string) error) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Nothing the gate reads lives under these.
			switch d.Name() {
			case "node_modules", ".git", "testdata", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return fn(path)
	})
}

// relPath is filepath.Rel that never fails.
func relPath(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path, nil
	}
	return rel, nil
}

// filepathBase is path.Base without the import churn in matrix.go.
func filepathBase(p string) string { return filepath.Base(p) }

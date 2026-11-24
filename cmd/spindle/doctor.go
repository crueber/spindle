// doctor.go — `spindle doctor`: the read-only readiness check run before
// (and after) an upgrade. It validates the deployed config with the shipped
// validators and samples the bucket with bounded LISTs, then reports one line
// per finding. No writes, no repairs.
//
// Exit codes (11_config_cli.md §6.7): 0 clean, 1 findings, 2 usage/IO failure.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/doctor"
	"github.com/crueber/spindle/internal/store"
)

func runDoctor(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("doctor")
	limit := fs.Int("limit", doctor.DefaultLimit, "objects decoded per family (and orphan rows per check)")
	repos := fs.Int("repos", doctor.DefaultRepos, "repositories sampled from repos/")
	budget := fs.Int("budget", doctor.DefaultBudget, "total objects decoded across all families")
	asJSON := fs.Bool("json", false, "print the stable JSON document instead of lines")
	mustParse(fs, args)
	if *limit < 1 || *repos < 1 || *budget < 1 {
		fmt.Fprintln(c.stderr(), "spindle: doctor: --limit, --repos and --budget must be >= 1")
		return exitArg
	}
	opts := doctor.Options{Limit: *limit, Repos: *repos, Budget: *budget}
	rep := doctor.NewReport(opts)

	// Config preflight through the shipped ladder. resolveConfigEff keeps the
	// best-effort config so a single bad key does not stop the bucket checks.
	cfg, state, overrides, loadErr := resolveConfigEff(c, os.Getenv)
	if state == stateAbsent && loadErr != nil {
		fmt.Fprintf(c.stderr(), "spindle: %s\n", loadErr.Error())
		return exitArg // missing explicitly-named config file: usage/IO, not a finding
	}
	if loadErr != nil && cfg == nil {
		// An unreadable file is an IO failure, not a finding: nothing can be
		// said about a config that could not be read.
		var pathErr *os.PathError
		if errors.As(loadErr, &pathErr) {
			fmt.Fprintf(c.stderr(), "spindle: %v\n", loadErr)
			return exitArg
		}
		// Strict decode failed (unknown/removed key, TOML error): reuse the
		// restricted-boot loader, which reports every key and still yields the
		// parseable values.
		best, errs := config.LoadSetupOnly(dataDirFor(c), configCandidates(c), os.Getenv)
		cfg = best
		keyed := doctor.LoadFindings(errs)
		if len(keyed) == 0 {
			// The best-effort loader dropped the reason (or returned nothing):
			// never lose the strict load's diagnosis.
			keyed = doctor.LoadFindings([]error{loadErr})
		}
		rep.Add(keyed...)
		loadErr = nil // already reported above
	}
	rep.Add(doctor.OverrideFindings(overrides)...)
	rep.Add(doctor.ValidateConfigWithLoad(cfg, loadErr)...)

	// Bucket sample. A store that cannot be opened is an IO failure (2), not a
	// finding: nothing was checked, so claiming "findings" would be a lie.
	//
	// The zero-config state is the exception: an unconfigured host has no
	// configured store yet (the restricted boot never creates one), so the
	// config findings are the whole answer. Reporting "store root missing" as
	// an IO failure there would hide the one thing the operator must fix.
	if cfg != nil && state != stateUnconfigured {
		st, err := openStoreReadOnly(cfg, dataDirFor(c))
		if err != nil {
			fmt.Fprintf(c.stderr(), "spindle: store: %v\n", err)
			return exitArg
		}
		bucket, err := doctor.Run(ctx, st, opts)
		if err != nil {
			fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
			return exitArg
		}
		rep.Add(bucket.Findings...)
		rep.Families = bucket.Families
		rep.Summary.Decoded = bucket.Summary.Decoded
		rep.Summary.BudgetExhausted = bucket.Summary.BudgetExhausted
		rep.Summary.ReposSampled = bucket.Summary.ReposSampled
		rep.Summary.ReposTotal = bucket.Summary.ReposTotal
		rep.Summary.ReposTruncated = bucket.Summary.ReposTruncated
	}

	if *asJSON {
		enc := json.NewEncoder(c.stdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(c.stderr(), "spindle: encode report: %v\n", err)
			return exitArg
		}
	} else if err := rep.WriteText(c.stdout()); err != nil {
		fmt.Fprintf(c.stderr(), "spindle: write report: %v\n", err)
		return exitArg
	}
	return doctorExitCode(rep)
}

// doctorExitCode is the §6.7 contract as a function the tests can pin:
// 0 clean, 1 findings. (A usage/IO failure returns exitArg before this point.)
func doctorExitCode(rep *doctor.Report) int {
	if rep.Clean() {
		return exitOK
	}
	return exitErr
}

// openStoreReadOnly builds the configured backend without the server's
// MkdirAll: the doctor must not create a store root, a cache dir or anything
// else. A missing filesystem root is an IO failure the operator should see.
func openStoreReadOnly(cfg *config.Config, dataDir string) (store.ObjectStore, error) {
	switch cfg.Store.Backend {
	case "memory":
		return store.NewMemory(), nil
	case "filesystem":
		root := cfg.Store.Root
		if root == "" {
			root = filepath.Join(dataDir, "store")
		}
		if _, err := os.Stat(root); err != nil {
			return nil, fmt.Errorf("store root %s: %w", root, err)
		}
		return store.NewFilesystemRoot(root, store.DefaultBulkConcurrency)
	case "s3":
		return store.NewS3(&cfg.Store)
	default:
		return nil, fmt.Errorf("unknown store.backend %q", cfg.Store.Backend)
	}
}

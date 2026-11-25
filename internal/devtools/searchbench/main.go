// searchbench measures the code-search primitive (04_git.md §17,
// docs/EVIDENCE.md E21): a real `git grep` subprocess over a synthetic bare
// repository, at several file counts and for the query shapes the service
// offers.
//
//	go run ./internal/devtools/searchbench                       # 1k + 10k files
//	go run ./internal/devtools/searchbench -sizes 1000,10000,50000 -reps 7
//
// Every measured run is the shipped path: *git.Layer.Grep (the pinned argv,
// the streaming -z parser and the caps). The fixture is a real bare repo built
// by `git fast-import` in a temp directory, removed on exit.
//
// The percentiles are environment-bound (CPU, page cache, filesystem); the
// durable shape claim is whether a query's cost grows with the file count.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/git"
)

func main() {
	sizes := flag.String("sizes", "1000,10000", "comma-separated file counts")
	lines := flag.Int("lines", 40, "lines per file")
	reps := flag.Int("reps", 7, "timed repetitions per query (after one warmup)")
	root := flag.String("root", "", "fixture root (default: a temp dir under os.TempDir())")
	keep := flag.Bool("keep", false, "keep the fixture directory")
	flag.Parse()

	fileSizes, err := parseSizes(*sizes)
	if err != nil {
		fatal(err)
	}
	dir, err := os.MkdirTemp(*root, "spindle-searchbench-")
	if err != nil {
		fatal(err)
	}
	if *keep {
		fmt.Printf("fixture root: %s\n", dir)
	} else {
		defer os.RemoveAll(dir)
	}

	fmt.Printf("host: %s/%s, %d CPUs, %s\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), gitVersion())
	fmt.Printf("fixture: bare repo built by git fast-import, %d lines/file, page cache warm after 1 untimed run\n\n", *lines)
	fmt.Println("| files | lines/file | query | matches | bytes | p50 | p95 |")
	fmt.Println("|---|---|---|---|---|---|---|")

	ctx := context.Background()
	for _, n := range fileSizes {
		bare, sha, err := buildFixture(dir, n, *lines)
		if err != nil {
			fatal(err)
		}
		layer := git.NewLayer()
		repo := &git.LocalRepo{Path: bare}
		for _, q := range queries() {
			// One untimed warmup so page cache and git's own caches are warm;
			// timings after it measure the steady state a second user sees.
			warm, err := layer.Grep(ctx, repo, sha, q.Query)
			if err != nil {
				fatal(fmt.Errorf("%d files, %s: %w", n, q.Name, err))
			}
			var durs []time.Duration
			for i := 0; i < *reps; i++ {
				start := time.Now()
				if _, err := layer.Grep(ctx, repo, sha, q.Query); err != nil {
					fatal(fmt.Errorf("%d files, %s: %w", n, q.Name, err))
				}
				durs = append(durs, time.Since(start))
			}
			sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
			fmt.Printf("| %d | %d | %s | %d | %d | %s | %s |\n",
				n, *lines, q.Name, warm.Matches, warm.Bytes,
				durs[len(durs)/2].Round(100*time.Microsecond),
				percentile(durs, 95).Round(100*time.Microsecond))
		}
	}
}

type benchQuery struct {
	Name  string
	Query git.GrepQuery
}

// queries is the product's query shape: a fixed string that exists in exactly
// one file (a full tree walk), the same shape as a POSIX ERE, and a broad
// string that trips the match caps almost immediately.
func queries() []benchQuery {
	return []benchQuery{
		{Name: "fixed narrow (1 file)", Query: git.GrepQuery{Pattern: "narrowneedle"}},
		{Name: "regex narrow (1 file)", Query: git.GrepQuery{Pattern: "n[a]rrownee[d]le", Regex: true}},
		{Name: "fixed broad (capped)", Query: git.GrepQuery{Pattern: "benchmarktoken"}},
	}
}

// buildFixture writes a bare repository with n files of `lines` lines each.
// File 0 carries "narrowneedle"; every line carries "benchmarktoken".
func buildFixture(root string, n, lines int) (bare, sha string, err error) {
	bare = fmt.Sprintf("%s/bench-%d-%d.git", root, n, lines)
	if out, ierr := exec.Command("git", "init", "--bare", "-q", "-b", "main", bare).CombinedOutput(); ierr != nil {
		return "", "", fmt.Errorf("git init: %v: %s", ierr, out)
	}
	var stream bytes.Buffer
	now := time.Now().Unix()
	msg := "bench fixture\n"
	fmt.Fprintf(&stream, "commit refs/heads/main\ncommitter Search Bench <bench@example.com> %d +0000\n", now)
	fmt.Fprintf(&stream, "data %d\n%s", len(msg), msg)
	for f := 0; f < n; f++ {
		var body strings.Builder
		for l := 0; l < lines; l++ {
			if f == 0 && l == 0 {
				body.WriteString("narrowneedle ")
			}
			fmt.Fprintf(&body, "line %d of file %d benchmarktoken lorem ipsum dolor sit amet\n", l, f)
		}
		content := body.String()
		fmt.Fprintf(&stream, "M 100644 inline dir%02d/file%05d.txt\n", f%100, f)
		fmt.Fprintf(&stream, "data %d\n%s", len(content), content)
	}
	cmd := exec.Command("git", "--git-dir="+bare, "fast-import", "--quiet")
	cmd.Stdin = &stream
	if out, ierr := cmd.CombinedOutput(); ierr != nil {
		return "", "", fmt.Errorf("git fast-import: %v: %s", ierr, out)
	}
	out, ierr := exec.Command("git", "--git-dir="+bare, "rev-parse", "HEAD").Output()
	if ierr != nil {
		return "", "", fmt.Errorf("rev-parse: %w", ierr)
	}
	return bare, strings.TrimSpace(string(out)), nil
}

func parseSizes(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("bad -sizes entry %q", part)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-sizes is empty")
	}
	return out, nil
}

// percentile returns the nearest-rank p-th percentile of a sorted slice.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func gitVersion() string {
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return "git (version unknown)"
	}
	return strings.TrimSpace(string(out))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "searchbench:", err)
	os.Exit(1)
}

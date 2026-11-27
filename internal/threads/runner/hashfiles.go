package runner

// hashFiles (PAR-041, docs/threads.md §3.7).
//
// The walk happens INSIDE the job container: the workspace is a docker volume
// the runner process does not mount, so the deterministic file list and the
// content hashes are produced by one `sh` script. The runner converts the
// workflow's glob patterns to ERE, sends them base64-encoded (never spliced
// into shell text), and hashes a sorted `<path> <sha256>` stream.
//
// The rules are documented and pinned:
//   - patterns are workspace-relative; `*`/`?` never cross `/`, `**` does;
//   - a leading `!` is an exclude pattern;
//   - only regular files are hashed (a symlink is not followed);
//   - the result is the SHA-256 of the sorted `sha256sum` lines, so the same
//     bytes and paths always produce the same value;
//   - no match is the EMPTY STRING, never an error;
//   - a walk over the file or byte cap is a named error (the cap is named in the
//     message), so a repository cannot turn one condition into an unbounded
//     read.

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// HashFiles caps. They are exported for the docs/tests and deliberately small:
// hashFiles is a cache key, not an archive.
const (
	MaxHashFiles     = 1000
	MaxHashFileBytes = 100 << 20 // 100 MiB
)

// hashFilesCache caches one job's results. A completed step may change the
// workspace, so the executor bumps the generation after each step.
type hashFilesCache struct {
	mu    sync.Mutex
	gen   int
	cache map[string]string
}

func newHashFilesCache() *hashFilesCache {
	return &hashFilesCache{cache: map[string]string{}}
}

func (c *hashFilesCache) invalidate() {
	c.mu.Lock()
	c.gen++
	c.cache = map[string]string{}
	c.mu.Unlock()
}

func (c *hashFilesCache) key(patterns []string) string {
	sorted := append([]string{}, patterns...)
	sort.Strings(sorted)
	return strconv.Itoa(c.gen) + "\x00" + strings.Join(sorted, "\x00")
}

func (c *hashFilesCache) get(patterns []string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	val, ok := c.cache[c.key(patterns)]
	return val, ok
}

func (c *hashFilesCache) put(patterns []string, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) > 32 {
		c.cache = map[string]string{}
	}
	c.cache[c.key(patterns)] = value
}

// hashFilesFunc builds the evaluator callback for one job attempt. The closure
// captures the job context and the plan, and is safe for the concurrent
// evaluation of a parallel step group.
func (e *Executor) hashFilesFunc(ctx context.Context, plan *Plan, cache *hashFilesCache) func([]string) (string, error) {
	return func(patterns []string) (string, error) {
		if cache != nil {
			if val, ok := cache.get(patterns); ok {
				return val, nil
			}
		}
		value, err := e.runHashFiles(ctx, plan, patterns)
		if err != nil {
			return "", err
		}
		if cache != nil {
			cache.put(patterns, value)
		}
		return value, nil
	}
}

// runHashFiles executes the walk inside the job container.
func (e *Executor) runHashFiles(ctx context.Context, plan *Plan, patterns []string) (string, error) {
	if plan == nil || plan.Names.Container == "" {
		return "", fmt.Errorf("hashFiles: the job container is not running yet")
	}
	if len(patterns) == 0 {
		return "", fmt.Errorf("hashFiles: needs at least one pattern")
	}
	regexes := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		re, err := globToERE(pattern)
		if err != nil {
			return "", err
		}
		regexes = append(regexes, re)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(strings.Join(regexes, "\n")))
	script := hashFilesScript(encoded, plan.Workspace, hashFilesNonce())
	stdout, stderr, code, err := e.Docker.Run(ctx, nil, "exec", plan.Names.Container, "sh", "-c", script)
	if err != nil && code == 0 {
		return "", err
	}
	if code != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = strings.TrimSpace(stdout)
		}
		if msg == "" {
			msg = fmt.Sprintf("exit %d", code)
		}
		return "", fmt.Errorf("hashFiles: %s", msg)
	}
	return strings.TrimSpace(stdout), nil
}

// hashFilesSeq makes each walk's temp paths unique: two parallel steps share a
// container and /tmp, so a fixed name would let them clobber each other.
var hashFilesSeq atomic.Uint64

func hashFilesNonce() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), hashFilesSeq.Add(1))
}

// hashFilesScript is the in-container walk. encoded holds one ERE per line,
// with a leading `!` for an exclude pattern.
func hashFilesScript(encoded, workspace, nonce string) string {
	ws := shellQuote(workspace)
	tmp := shellQuote("/tmp/spindle-hashfiles-" + nonce)
	return `set -u
TMP=` + tmp + `
cd ` + ws + ` || { echo "hashFiles: workspace is not readable" >&2; exit 2; }
command -v sha256sum >/dev/null 2>&1 || { echo "hashFiles: sha256sum is not available in the job image" >&2; exit 5; }
echo ` + shellQuote(encoded) + ` | base64 -d > "$TMP.patterns"
find . -type f -print | sed 's|^\./||' | LC_ALL=C sort > "$TMP.all"
LC_ALL=C awk -v patterns="$TMP.patterns" '
BEGIN {
  while ((getline line < patterns) > 0) {
    neg = 0
    if (substr(line, 1, 1) == "!") { neg = 1; line = substr(line, 2) }
    if (line == "") continue
    n++; pat[n] = line; excluded[n] = neg
  }
}
{
  include = 0; drop = 0
  for (i = 1; i <= n; i++) {
    if ($0 ~ pat[i]) { if (excluded[i]) drop = 1; else include = 1 }
  }
  if (include && !drop) print
}' "$TMP.all" > "$TMP.match"
count=$(wc -l < "$TMP.match" | tr -d ' ')
if [ "$count" -gt ` + strconv.Itoa(MaxHashFiles) + ` ]; then
  echo "hashFiles: matched $count files, over the cap of ` + strconv.Itoa(MaxHashFiles) + `" >&2
  exit 3
fi
total=0
while IFS= read -r file; do
  size=$(wc -c < "$file" | tr -d ' ')
  total=$((total + size))
done < "$TMP.match"
if [ "$total" -gt ` + strconv.Itoa(MaxHashFileBytes) + ` ]; then
  echo "hashFiles: matched $total bytes, over the cap of ` + strconv.Itoa(MaxHashFileBytes) + `" >&2
  exit 4
fi
if [ "$count" -eq 0 ]; then
  exit 0
fi
while IFS= read -r file; do
  sha256sum -- "$file"
done < "$TMP.match" | sha256sum | awk '{print $1}'
`
}

// globToERE converts one hashFiles glob to the ERE the in-container awk uses.
// A leading `!` becomes an exclude marker. `*`/`?` stay inside one path
// segment; `**` crosses segments (and `**/` may match no directory at all).
func globToERE(pattern string) (string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", fmt.Errorf("hashFiles: an empty pattern is not valid")
	}
	exclude := false
	if strings.HasPrefix(pattern, "!") {
		exclude = true
		pattern = strings.TrimPrefix(pattern, "!")
	}
	pattern = strings.TrimPrefix(pattern, "./")
	if pattern == "" {
		return "", fmt.Errorf("hashFiles: an empty pattern is not valid")
	}
	var b strings.Builder
	if exclude {
		b.WriteByte('!')
	}
	b.WriteByte('^')
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			// Copy a character class as authored, translating `!` to `^`.
			j := i + 1
			if j < len(pattern) && (pattern[j] == '!' || pattern[j] == '^') {
				b.WriteByte('[')
				b.WriteByte('^')
				j++
			} else {
				b.WriteByte('[')
			}
			closed := false
			for ; j < len(pattern); j++ {
				if pattern[j] == ']' && j > i+1 {
					b.WriteByte(']')
					closed = true
					j++
					break
				}
				b.WriteByte(pattern[j])
			}
			if !closed {
				return "", fmt.Errorf("hashFiles: pattern %q has an unterminated character class", pattern)
			}
			i = j - 1
		case '.', '+', '(', ')', '{', '}', '^', '$', '|', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('$')
	return b.String(), nil
}

// shellQuote single-quotes one string for `sh -c`.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

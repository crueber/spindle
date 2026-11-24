package branches

// git.go — the one git read branches performs, run exactly as 04_git.md §16.2
// pins it: in the repo's serving copy, under a ctx timeout, with
// GIT_TERMINAL_PROMPT=0 and --end-of-options so a user-supplied rev is never
// parsed as an option.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// SubprocessGit implements GitRunner with the stock git binary.
type SubprocessGit struct {
	Binary  string
	Timeout time.Duration
}

// NewSubprocessGit builds a resolver (30 s default per spawn).
func NewSubprocessGit(binary string) *SubprocessGit {
	if binary == "" {
		binary = "git"
	}
	return &SubprocessGit{Binary: binary, Timeout: ResolveTimeout}
}

// ResolveCommit resolves a rev to its commit sha:
//
//	git rev-parse --verify --quiet --end-of-options <rev>^{commit}
func (g *SubprocessGit) ResolveCommit(ctx context.Context, dir, rev string) (string, error) {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		return "", fmt.Errorf("%w: empty revision", ErrInvalid)
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = ResolveTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.Binary, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_DIR="+dir)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: rev-parse timed out: %v", ErrUnavailable, ctx.Err())
		}
		return "", fmt.Errorf("rev-parse %s: %v: %s", rev, err, strings.TrimSpace(stderr.String()))
	}
	sha := strings.TrimSpace(out.String())
	if sha == "" {
		return "", fmt.Errorf("rev-parse %s: no object", rev)
	}
	return sha, nil
}

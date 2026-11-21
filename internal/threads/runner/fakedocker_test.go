package runner

import (
	"context"
	"io"
	"strings"
	"sync"
)

// fakeDocker is the subprocess-boundary recorder: it asserts argv exactly and
// scripts exit codes without a daemon (15 §7's "fake docker CLI" test shape).
type fakeDocker struct {
	mu sync.Mutex
	// calls are the argv slices, joined for assertions.
	calls []string
	// envs are the extra environments passed to each call (parallel to calls).
	envs [][]string
	// streamOutput is written by Stream; streamCode is its exit code.
	streamOutput string
	streamCode   int
	// runOutputs maps a substring of the joined argv to (stdout, code).
	runOutputs []runOutput
	// blockOn is a substring of the argv whose Stream call waits for ctx.Done.
	blockOn string
	// streamByMatch scripts per-command exit codes for Stream calls.
	streamByMatch []runOutput
}

type runOutput struct {
	match string
	out   string
	code  int
}

func (f *fakeDocker) Run(ctx context.Context, env []string, argv ...string) (string, string, int, error) {
	joined := strings.Join(argv, " ")
	f.mu.Lock()
	f.calls = append(f.calls, joined)
	f.envs = append(f.envs, append([]string(nil), env...))
	outputs := append([]runOutput(nil), f.runOutputs...)
	f.mu.Unlock()
	for _, o := range outputs {
		if strings.Contains(joined, o.match) {
			return o.out, "", o.code, nil
		}
	}
	return "", "", 0, nil
}

func (f *fakeDocker) Stream(ctx context.Context, env []string, w io.Writer, argv ...string) (int, error) {
	joined := strings.Join(argv, " ")
	f.mu.Lock()
	f.calls = append(f.calls, joined)
	f.envs = append(f.envs, append([]string(nil), env...))
	out, code, block := f.streamOutput, f.streamCode, f.blockOn
	matches := append([]runOutput(nil), f.streamByMatch...)
	f.mu.Unlock()
	for _, o := range matches {
		if strings.Contains(joined, o.match) {
			code = o.code
		}
	}
	if block != "" && strings.Contains(joined, block) {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	if out != "" {
		_, _ = io.WriteString(w, out)
	}
	return code, nil
}

// called reports whether an argv with this substring ran.
func (f *fakeDocker) called(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// count counts calls containing sub.
func (f *fakeDocker) count(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// snapshot returns the recorded calls.
func (f *fakeDocker) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

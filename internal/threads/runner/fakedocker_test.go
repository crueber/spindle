package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
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
	// streamInOutput is written by StreamIn after it drains stdin (the upload
	// tar fixture); streamInErr makes StreamIn fail after draining.
	streamInOutput []byte
	streamInErr    error
	// streamInSinks receives what StreamIn's stdin carried (the download tar).
	streamInSinks []*strings.Builder
	// cpFiles maps a /tmp basename to its body; a `cp <name>:/tmp <dest>` call
	// materializes them under dest (a docker action's output file).
	cpFiles map[string]string
}

type runOutput struct {
	match string
	out   string
	// errOut is stderr (where docker prints a pull failure). Empty for a
	// stdout-only scripted answer.
	errOut string
	code   int
}

func (f *fakeDocker) Run(ctx context.Context, env []string, argv ...string) (string, string, int, error) {
	joined := strings.Join(argv, " ")
	f.mu.Lock()
	f.calls = append(f.calls, joined)
	f.envs = append(f.envs, append([]string(nil), env...))
	outputs := append([]runOutput(nil), f.runOutputs...)
	files := map[string]string{}
	for name, body := range f.cpFiles {
		files[name] = body
	}
	f.mu.Unlock()
	// `docker cp <container>:/tmp <dest>`: materialize the action's outputs so
	// the collector reads a real file, exactly as the CLI would.
	if len(argv) >= 3 && argv[0] == "cp" && strings.Contains(argv[1], ":/tmp") && len(files) > 0 {
		if err := os.MkdirAll(argv[2], 0o755); err != nil {
			return "", err.Error(), 1, nil
		}
		for name, body := range files {
			if werr := os.WriteFile(filepath.Join(argv[2], name), []byte(body), 0o644); werr != nil {
				return "", werr.Error(), 1, nil
			}
		}
		return "", "", 0, nil
	}
	for _, o := range outputs {
		if strings.Contains(joined, o.match) {
			return o.out, o.errOut, o.code, nil
		}
	}
	return "", "", 0, nil
}

// Stream implements DockerCLI.
func (f *fakeDocker) Stream(ctx context.Context, env []string, w io.Writer, argv ...string) (int, error) {
	return f.StreamIn(ctx, env, nil, w, argv...)
}

// StreamIn implements DockerCLI: it records the call and either consumes stdin
// into a sink (a download's tar) or emits a scripted archive on stdout (an
// upload's tar).
func (f *fakeDocker) StreamIn(ctx context.Context, env []string, r io.Reader, w io.Writer, argv ...string) (int, error) {
	joined := strings.Join(argv, " ")
	f.mu.Lock()
	f.calls = append(f.calls, joined)
	f.envs = append(f.envs, append([]string(nil), env...))
	out, code, block := f.streamOutput, f.streamCode, f.blockOn
	matches := append([]runOutput(nil), f.streamByMatch...)
	inOutput := append([]byte(nil), f.streamInOutput...)
	inErr := f.streamInErr
	sink := &strings.Builder{}
	f.streamInSinks = append(f.streamInSinks, sink)
	f.mu.Unlock()
	if r != nil {
		_, _ = io.Copy(sink, r)
	}
	for _, o := range matches {
		if strings.Contains(joined, o.match) {
			code = o.code
		}
	}
	if block != "" && strings.Contains(joined, block) {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	if len(inOutput) > 0 {
		_, _ = w.Write(inOutput)
	}
	if out != "" {
		_, _ = io.WriteString(w, out)
	}
	return code, inErr
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

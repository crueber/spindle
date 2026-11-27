package runner

// Registry authentication (PAR-078; docs/threads.md §8.6).
//
// The pull identity belongs to the RUNNER, never to a workflow. A private job or
// service image is reachable because the runner operator configured a registry
// credential, not because the workflow carries `credentials:` — a
// workflow-supplied password would travel in the job payload (the bucket) and in
// docker create argv, which is exactly what N7 forbids.
//
// The credential file is `registry username password` per line (0600; a
// group/world-readable file is refused). At startup the runner logs in ONCE into
// a private temporary DOCKER_CONFIG directory, passing the password on stdin
// (`docker login --password-stdin`) — never argv, never the environment of a
// child it did not build. Every later docker call points DOCKER_CONFIG at that
// directory, so `docker create --pull=always` and a service's pull authenticate
// without the workflow seeing anything. Each password joins the log mask list,
// so an accidental echo is redacted like a repository secret. The directory is
// removed when the runner stops.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// RegistryAuth is one prepared private docker-config directory.
type RegistryAuth struct {
	// ConfigDir is the temporary DOCKER_CONFIG directory (0700).
	ConfigDir string
	// masks are the passwords, joined to every job's log mask list.
	masks []string
}

// Env is the environment every docker CLI call must carry.
func (a *RegistryAuth) Env() []string {
	if a == nil || a.ConfigDir == "" {
		return nil
	}
	return []string{"DOCKER_CONFIG=" + a.ConfigDir}
}

// Masks returns the credential values a job log must redact.
func (a *RegistryAuth) Masks() []string {
	if a == nil {
		return nil
	}
	return append([]string(nil), a.masks...)
}

// Cleanup removes the temporary docker config directory.
func (a *RegistryAuth) Cleanup() {
	if a != nil && a.ConfigDir != "" {
		_ = os.RemoveAll(a.ConfigDir)
	}
}

// RegistryMasks is the Executor's view of the runner's credential values.
func (e *Executor) RegistryMasks() []string {
	if e == nil {
		return nil
	}
	return e.RegistryAuth.Masks()
}

// registryCredential is one parsed line of the credential file.
type registryCredential struct {
	registry string
	username string
	password string
}

// parseRegistryAuthFile parses `registry username password` lines. Blank lines
// and `#` comments are ignored; a malformed line is a startup error naming the
// line number (never its content).
func parseRegistryAuthFile(data []byte) ([]registryCredential, error) {
	var out []registryCredential
	for i, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		if len(fields) != 3 {
			// Tabs are a common typo for the separator; accept them too.
			fields = strings.SplitN(line, "\t", 3)
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("registry auth file line %d: expected `registry username password`", i+1)
		}
		cred := registryCredential{
			registry: strings.TrimSpace(fields[0]),
			username: strings.TrimSpace(fields[1]),
			password: strings.TrimSpace(fields[2]),
		}
		if cred.registry == "" || cred.username == "" || cred.password == "" {
			return nil, fmt.Errorf("registry auth file line %d: registry, username and password are all required", i+1)
		}
		out = append(out, cred)
	}
	return out, nil
}

// PrepareRegistryAuth reads the credential file, creates a private docker config
// and logs in to every named registry with the password on stdin. It is called
// once at runner startup; a failure is fatal (a configured credential that
// cannot log in would fail every private pull later).
func PrepareRegistryAuth(ctx context.Context, dockerBin, path string) (*RegistryAuth, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("registry auth file: %v", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("registry auth file %s is readable by group/other (mode %04o); make it 0600", path, st.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("registry auth file: %v", err)
	}
	creds, err := parseRegistryAuthFile(data)
	if err != nil {
		return nil, err
	}
	if len(creds) == 0 {
		return nil, fmt.Errorf("registry auth file %s names no registry", path)
	}
	dir, err := os.MkdirTemp("", "spindle-registry-*")
	if err != nil {
		return nil, fmt.Errorf("registry auth: create config dir: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("registry auth: secure config dir: %v", err)
	}
	auth := &RegistryAuth{ConfigDir: dir}
	for _, c := range creds {
		if err := dockerLogin(ctx, dockerBin, dir, c); err != nil {
			auth.Cleanup()
			return nil, err
		}
		auth.masks = append(auth.masks, c.password)
	}
	return auth, nil
}

// dockerLogin runs `docker --config <dir> login --username <user>
// --password-stdin <registry>` with the password on stdin. The password never
// appears in argv, the process environment, or an error message.
func dockerLogin(ctx context.Context, dockerBin, dir string, c registryCredential) error {
	if strings.TrimSpace(dockerBin) == "" {
		dockerBin = "docker"
	}
	loginCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(loginCtx, dockerBin, "login",
		"--username", c.username, "--password-stdin", c.registry)
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+dir)
	cmd.Stdin = strings.NewReader(c.password + "\n")
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		// The registry/user are not secrets; the password is never in any
		// captured output we echo, but scrubbing it is still the rule.
		detail := strings.TrimSpace(out.String())
		detail = strings.ReplaceAll(detail, c.password, "***")
		return fmt.Errorf("registry auth: docker login %s failed: %v: %s", c.registry, err, detail)
	}
	return nil
}
